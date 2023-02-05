package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/NYTimes/gziphandler"
	sdkPlugin "github.com/gatewayd-io/gatewayd-plugin-sdk/plugin"
	"github.com/gatewayd-io/gatewayd/config"
	gerr "github.com/gatewayd-io/gatewayd/errors"
	"github.com/gatewayd-io/gatewayd/logging"
	"github.com/gatewayd-io/gatewayd/metrics"
	"github.com/gatewayd-io/gatewayd/network"
	"github.com/gatewayd-io/gatewayd/plugin"
	"github.com/gatewayd-io/gatewayd/pool"
	"github.com/getsentry/sentry-go"
	"github.com/go-co-op/gocron"
	"github.com/panjf2000/gnet/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

var (
	enableSentry     bool
	pluginConfigFile string
	globalConfigFile string
	conf             *config.Config
	pluginRegistry   *plugin.Registry

	loggers              = make(map[string]zerolog.Logger)
	healthCheckScheduler = gocron.NewScheduler(time.UTC)
)

// runCmd represents the run command.
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a gatewayd instance",
	Run: func(cmd *cobra.Command, args []string) {
		// Enable Sentry.
		if enableSentry {
			// Initialize Sentry.
			err := sentry.Init(sentry.ClientOptions{
				Dsn:              "https://e22f42dbb3e0433fbd9ea32453faa598@o4504550475038720.ingest.sentry.io/4504550481723392",
				TracesSampleRate: config.DefaultTraceSampleRate,
				AttachStacktrace: config.DefaultAttachStacktrace,
			})
			if err != nil {
				log.Fatalf("sentry.Init: %s", err)
			}

			// Flush buffered events before the program terminates.
			defer sentry.Flush(config.DefaultFlushTimeout)
			// Recover from panics and report the error to Sentry.
			defer sentry.Recover()
		}

		// Load global and plugin configuration.
		conf = config.NewConfig(globalConfigFile, pluginConfigFile)

		// Create and initialize loggers from the config.
		for name, cfg := range conf.Global.Loggers {
			loggers[name] = logging.NewLogger(logging.LoggerConfig{
				Output:            cfg.GetOutput(),
				Level:             cfg.GetLevel(),
				TimeFormat:        cfg.GetTimeFormat(),
				ConsoleTimeFormat: cfg.GetConsoleTimeFormat(),
				NoColor:           cfg.NoColor,
				FileName:          cfg.FileName,
				MaxSize:           cfg.MaxSize,
				MaxBackups:        cfg.MaxBackups,
				MaxAge:            cfg.MaxAge,
				Compress:          cfg.Compress,
				LocalTime:         cfg.LocalTime,
				SyslogPriority:    cfg.GetSyslogPriority(),
				RSyslogNetwork:    cfg.RSyslogNetwork,
				RSyslogAddress:    cfg.RSyslogAddress,
			})
		}

		// Set the default logger.
		logger := loggers[config.Default]

		// Create a new plugin registry.
		// The plugins are loaded and hooks registered before the configuration is loaded.
		pluginRegistry = plugin.NewRegistry(config.Loose, config.PassDown, config.Accept, logger)
		// Set the plugin requirement's compatibility policy.
		pluginRegistry.Compatibility = conf.Plugin.GetPluginCompatibilityPolicy()
		// Set hooks' signature verification policy.
		pluginRegistry.Verification = conf.Plugin.GetVerificationPolicy()
		// Set custom hook acceptance policy.
		pluginRegistry.Acceptance = conf.Plugin.GetAcceptancePolicy()

		// Load plugins and register their hooks.
		pluginRegistry.LoadPlugins(conf.Plugin.Plugins)

		// Start the metrics merger.
		metricsMerger := metrics.NewMerger(conf.Plugin.MetricsMergerPeriod, logger)
		pluginRegistry.ForEach(func(_ sdkPlugin.Identifier, plugin *plugin.Plugin) {
			if metricsEnabled, err := strconv.ParseBool(plugin.Config["metricsEnabled"]); err == nil && metricsEnabled {
				metricsMerger.Add(plugin.ID.Name, plugin.Config["metricsUnixDomainSocket"])
			}
		})
		metricsMerger.Start()

		logger.Info().Str(
			"healthCheckPeriod", conf.Plugin.HealthCheckPeriod.String(),
		).Msg("Starting plugin health check scheduler")
		// Ping the plugins to check if they are alive, and remove them if they are not.
		startDelay := time.Now().Add(conf.Plugin.HealthCheckPeriod)
		if _, err := healthCheckScheduler.Every(
			conf.Plugin.HealthCheckPeriod).SingletonMode().StartAt(startDelay).Do(func() {
			pluginRegistry.ForEach(func(pluginId sdkPlugin.Identifier, plugin *plugin.Plugin) {
				if err := plugin.Ping(); err != nil {
					logger.Error().Err(err).Msg("Failed to ping plugin")
					metricsMerger.Remove(pluginId.Name)
					pluginRegistry.Remove(pluginId)
				} else {
					logger.Trace().Str("name", pluginId.Name).Msg("Successfully pinged plugin")
				}
			})
		}); err != nil {
			logger.Error().Err(err).Msg("Failed to start plugin health check scheduler")
		}
		healthCheckScheduler.StartAsync()

		// The config will be passed to the plugins that register to the "OnConfigLoaded" plugin.
		// The plugins can modify the config and return it.
		updatedGlobalConfig, err := pluginRegistry.Run(
			context.Background(),
			conf.GlobalKoanf.All(),
			sdkPlugin.OnConfigLoaded)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to run OnConfigLoaded hooks")
		}

		// If the config was modified by the plugins, merge it with the one loaded from the file.
		// Only global configuration is merged, which means that plugins cannot modify the plugin
		// configurations.
		if updatedGlobalConfig != nil {
			// Merge the config with the one loaded from the file (in memory).
			// The changes won't be persisted to disk.
			conf.MergeGlobalConfig(updatedGlobalConfig)
		}

		// Start the metrics server if enabled.
		go func(metricsConfig config.Metrics, logger zerolog.Logger) {
			// TODO: refactor this to a separate function.
			if !metricsConfig.Enabled {
				logger.Info().Msg("Metrics server is disabled")
				return
			}

			fqdn, err := url.Parse("http://" + metricsConfig.Address)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to parse metrics address")
				return
			}

			address, err := url.JoinPath(fqdn.String(), metricsConfig.Path)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to parse metrics path")
				return
			}

			// Merge the metrics from the plugins with the ones from GatewayD.
			mergedMetricsHandler := func(next http.Handler) http.Handler {
				handler := func(w http.ResponseWriter, r *http.Request) {
					if _, err := w.Write(metricsMerger.OutputMetrics); err != nil {
						logger.Error().Err(err).Msg("Failed to write metrics")
						sentry.CaptureException(err)
					}
					next.ServeHTTP(w, r)
				}
				return http.HandlerFunc(handler)
			}

			decompressedGatewayDMetricsHandler := func() http.Handler {
				return promhttp.InstrumentMetricHandler(
					prometheus.DefaultRegisterer,
					promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
						DisableCompression: true,
					}),
				)
			}

			logger.Info().Str("address", address).Msg("Metrics are exposed")
			http.Handle(
				metricsConfig.Path,
				gziphandler.GzipHandler(
					mergedMetricsHandler(
						decompressedGatewayDMetricsHandler(),
					),
				),
			)

			//nolint:gosec
			if err = http.ListenAndServe(
				metricsConfig.Address, nil); err != nil {
				logger.Error().Err(err).Msg("Failed to start metrics server")
			}
		}(conf.Global.Metrics[config.Default], logger)

		// This is a notification hook, so we don't care about the result.
		// TODO: Use a context with a timeout
		if data, ok := conf.GlobalKoanf.Get("loggers").(map[string]interface{}); ok {
			_, err = pluginRegistry.Run(context.Background(), data, sdkPlugin.OnNewLogger)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to run OnNewLogger hooks")
			}
		} else {
			logger.Error().Msg("Failed to get loggers from config")
		}

		// Create and initialize a pool of connections.
		poolSize := conf.Global.Pools[config.Default].GetSize()
		pool := pool.NewPool(poolSize)

		// Get client config from the config file.
		clientConfig := conf.Global.Clients[config.Default]

		// Add clients to the pool.
		for i := 0; i < poolSize; i++ {
			client := network.NewClient(&clientConfig, logger)

			if client != nil {
				clientCfg := map[string]interface{}{
					"id":                 client.ID,
					"network":            client.Network,
					"address":            client.Address,
					"receiveBufferSize":  client.ReceiveBufferSize,
					"receiveChunkSize":   client.ReceiveChunkSize,
					"receiveDeadline":    client.ReceiveDeadline.String(),
					"sendDeadline":       client.SendDeadline.String(),
					"tcpKeepAlive":       client.TCPKeepAlive,
					"tcpKeepAlivePeriod": client.TCPKeepAlivePeriod.String(),
				}
				_, err := pluginRegistry.Run(context.Background(), clientCfg, sdkPlugin.OnNewClient)
				if err != nil {
					logger.Error().Err(err).Msg("Failed to run OnNewClient hooks")
				}

				err = pool.Put(client.ID, client)
				if err != nil {
					logger.Error().Err(err).Msg("Failed to add client to the pool")
				}
			}
		}

		// Verify that the pool is properly populated.
		logger.Info().Str("count", fmt.Sprint(pool.Size())).Msg(
			"There are clients available in the pool")
		if pool.Size() != poolSize {
			logger.Error().Msg(
				"The pool size is incorrect, either because " +
					"the clients cannot connect due to no network connectivity " +
					"or the server is not running. exiting...")
			pluginRegistry.Shutdown()
			os.Exit(gerr.FailedToInitializePool)
		}

		_, err = pluginRegistry.Run(
			context.Background(),
			map[string]interface{}{"size": poolSize},
			sdkPlugin.OnNewPool)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to run OnNewPool hooks")
		}

		// Create a prefork proxy with the pool of clients.
		elastic := conf.Global.Proxy[config.Default].Elastic
		reuseElasticClients := conf.Global.Proxy[config.Default].ReuseElasticClients
		healthCheckPeriod := conf.Global.Proxy[config.Default].HealthCheckPeriod
		proxy := network.NewProxy(
			pool,
			pluginRegistry,
			elastic,
			reuseElasticClients,
			healthCheckPeriod,
			&clientConfig,
			logger,
		)

		if data, ok := conf.GlobalKoanf.Get("proxy").(map[string]interface{}); ok {
			_, err = pluginRegistry.Run(context.Background(), data, sdkPlugin.OnNewProxy)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to run OnNewProxy hooks")
			}
		} else {
			logger.Error().Msg("Failed to get proxy from config")
		}

		// Create a server
		serverCfg := conf.Global.Servers[config.Default]
		server := network.NewServer(
			serverCfg.Network,
			serverCfg.Address,
			serverCfg.SoftLimit,
			serverCfg.HardLimit,
			serverCfg.TickInterval,
			[]gnet.Option{
				// Scheduling options
				gnet.WithMulticore(serverCfg.MultiCore),
				gnet.WithLockOSThread(serverCfg.LockOSThread),
				// NumEventLoop overrides Multicore option.
				// gnet.WithNumEventLoop(1),

				// Can be used to send keepalive messages to the client.
				gnet.WithTicker(serverCfg.EnableTicker),

				// Internal event-loop load balancing options
				gnet.WithLoadBalancing(serverCfg.GetLoadBalancer()),

				// Buffer options
				gnet.WithReadBufferCap(serverCfg.ReadBufferCap),
				gnet.WithWriteBufferCap(serverCfg.WriteBufferCap),
				gnet.WithSocketRecvBuffer(serverCfg.SocketRecvBuffer),
				gnet.WithSocketSendBuffer(serverCfg.SocketSendBuffer),

				// TCP options
				gnet.WithReuseAddr(serverCfg.ReuseAddress),
				gnet.WithReusePort(serverCfg.ReusePort),
				gnet.WithTCPKeepAlive(serverCfg.TCPKeepAlive),
				gnet.WithTCPNoDelay(serverCfg.GetTCPNoDelay()),
			},
			proxy,
			logger,
			pluginRegistry,
		)

		if data, ok := conf.GlobalKoanf.Get("servers").(map[string]interface{}); ok {
			_, err = pluginRegistry.Run(context.Background(), data, sdkPlugin.OnNewServer)
			if err != nil {
				logger.Error().Err(err).Msg("Failed to run OnNewServer hooks")
			}
		} else {
			logger.Error().Msg("Failed to get the servers configuration")
		}

		// Shutdown the server gracefully.
		var signals []os.Signal
		signals = append(signals,
			os.Interrupt,
			os.Kill,
			syscall.SIGTERM,
			syscall.SIGABRT,
			syscall.SIGQUIT,
			syscall.SIGHUP,
			syscall.SIGINT,
		)
		signalsCh := make(chan os.Signal, 1)
		signal.Notify(signalsCh, signals...)
		go func(pluginRegistry *plugin.Registry, logger zerolog.Logger, server *network.Server) {
			for sig := range signalsCh {
				for _, s := range signals {
					if sig != s {
						logger.Info().Msg("Notifying the plugins that the server is shutting down")
						_, err := pluginRegistry.Run(
							context.Background(),
							map[string]interface{}{"signal": sig.String()},
							sdkPlugin.OnSignal,
						)
						if err != nil {
							logger.Error().Err(err).Msg("Failed to run OnSignal hooks")
						}

						logger.Info().Msg("Stopping GatewayD")
						healthCheckScheduler.Clear()
						logger.Info().Msg("Stopped health check scheduler")
						metricsMerger.Stop()
						logger.Info().Msg("Stopped metrics merger")
						server.Shutdown()
						logger.Info().Msg("Stopped server")
						pluginRegistry.Shutdown()
						logger.Info().Msg("Stopped plugin registry")
						os.Exit(0)
					}
				}
			}
		}(pluginRegistry, logger, server)

		// Run the server.
		if err := server.Run(); err != nil {
			logger.Error().Err(err).Msg("Failed to start server")
			metricsMerger.Stop()
			server.Shutdown()
			pluginRegistry.Shutdown()
			os.Exit(gerr.FailedToStartServer)
		}
	},
}

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(
		&globalConfigFile,
		"config", "c", "./gatewayd.yaml",
		"Global config file")
	runCmd.Flags().StringVarP(
		&pluginConfigFile,
		"plugin-config", "p", "./gatewayd_plugins.yaml",
		"Plugin config file")
	rootCmd.PersistentFlags().BoolVar(
		&enableSentry, "sentry", true, "Enable Sentry")
}
