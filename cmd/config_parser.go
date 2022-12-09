package cmd

import (
	"os"
	"time"

	"github.com/gatewayd-io/gatewayd/logging"
	"github.com/gatewayd-io/gatewayd/network"
	"github.com/knadh/koanf"
	"github.com/panjf2000/gnet/v2"
	"github.com/rs/zerolog"
)

// Global koanf instance. Using "." as the key path delimiter.
var globalConfig = koanf.New(".")

func getPath(path string) string {
	ref := globalConfig.String(path)
	if globalConfig.Exists(path) && globalConfig.StringMap(ref) != nil {
		return ref
	}

	return path
}

// func resolvePath(path string) map[string]string {
// 	ref := getPath(path)
// 	if ref != path {
// 		return konfig.StringMap(ref)
// 	}
// 	return nil
// }

func verificationPolicy() network.Policy {
	vPolicy := globalConfig.String("plugins.verificationPolicy")
	verificationPolicy := network.PassDown // default
	switch vPolicy {
	case "ignore":
		verificationPolicy = network.Ignore
	case "abort":
		verificationPolicy = network.Abort
	case "remove":
		verificationPolicy = network.Remove
	}

	return verificationPolicy
}

func loggerConfig() logging.LoggerConfig {
	cfg := logging.LoggerConfig{}
	switch globalConfig.String("loggers.logger.output") {
	case "stdout":
		cfg.Output = os.Stdout
	case "console":
	default:
		cfg.Output = nil
	}

	switch globalConfig.String("loggers.logger.timeFormat") {
	case "unixms":
		cfg.TimeFormat = zerolog.TimeFormatUnixMs
	case "unixmicro":
		cfg.TimeFormat = zerolog.TimeFormatUnixMicro
	case "unixnano":
		cfg.TimeFormat = zerolog.TimeFormatUnixNano
	case "unix":
		cfg.TimeFormat = zerolog.TimeFormatUnix
	default:
		cfg.TimeFormat = zerolog.TimeFormatUnix
	}

	switch globalConfig.String("loggers.logger.level") {
	case "debug":
		cfg.Level = zerolog.DebugLevel
	case "info":
		cfg.Level = zerolog.InfoLevel
	case "warn":
		cfg.Level = zerolog.WarnLevel
	case "error":
		cfg.Level = zerolog.ErrorLevel
	case "fatal":
		cfg.Level = zerolog.FatalLevel
	case "panic":
		cfg.Level = zerolog.PanicLevel
	case "disabled":
		cfg.Level = zerolog.Disabled
	case "trace":
		cfg.Level = zerolog.TraceLevel
	default:
		cfg.Level = zerolog.InfoLevel
	}

	cfg.NoColor = globalConfig.Bool("loggers.logger.noColor")

	return cfg
}

func poolConfig() (int, *network.Client) {
	poolSize := globalConfig.Int("pool.size")
	if poolSize == 0 {
		poolSize = network.DefaultPoolSize
	}

	// Minimum pool size is 2.
	if poolSize < 2 {
		poolSize = network.MinimumPoolSize
	}

	ref := getPath("pool.client")
	net := globalConfig.String(ref + ".network")
	address := globalConfig.String(ref + ".address")
	receiveBufferSize := globalConfig.Int(ref + ".receiveBufferSize")

	return poolSize, &network.Client{
		Network:           net,
		Address:           address,
		ReceiveBufferSize: receiveBufferSize,
	}
}

func proxyConfig() (bool, bool, *network.Client) {
	elastic := globalConfig.Bool("proxy.elastic")
	reuseElasticClients := globalConfig.Bool("proxy.reuseElasticClients")

	ref := getPath("pool.client")
	net := globalConfig.String(ref + ".network")
	address := globalConfig.String(ref + ".address")
	receiveBufferSize := globalConfig.Int(ref + ".receiveBufferSize")

	return elastic, reuseElasticClients, &network.Client{
		Network:           net,
		Address:           address,
		ReceiveBufferSize: receiveBufferSize,
	}
}

type ServerConfig struct {
	Network          string
	Address          string
	SoftLimit        uint64
	HardLimit        uint64
	EnableTicker     bool
	MultiCore        bool
	LockOSThread     bool
	ReuseAddress     bool
	ReusePort        bool
	LoadBalancer     gnet.LoadBalancing
	TickInterval     time.Duration
	ReadBufferCap    int
	WriteBufferCap   int
	SocketRecvBuffer int
	SocketSendBuffer int
	TCPKeepAlive     time.Duration
	TCPNoDelay       gnet.TCPSocketOpt
}

var loadBalancer = map[string]gnet.LoadBalancing{
	"roundrobin":       gnet.RoundRobin,
	"leastconnections": gnet.LeastConnections,
	"sourceaddrhash":   gnet.SourceAddrHash,
}

func getLoadBalancer(name string) gnet.LoadBalancing {
	if lb, ok := loadBalancer[name]; ok {
		return lb
	}

	return gnet.RoundRobin
}

func getTCPNoDelay() gnet.TCPSocketOpt {
	if globalConfig.Bool("server.tcpNoDelay") {
		return gnet.TCPNoDelay
	}

	return gnet.TCPDelay
}

func serverConfig() *ServerConfig {
	return &ServerConfig{
		Network:          globalConfig.String("server.network"),
		Address:          globalConfig.String("server.address"),
		SoftLimit:        uint64(globalConfig.Int64("server.softLimit")),
		HardLimit:        uint64(globalConfig.Int64("server.hardLimit")),
		EnableTicker:     globalConfig.Bool("server.enableTicker"),
		TickInterval:     globalConfig.Duration("server.tickInterval"),
		MultiCore:        globalConfig.Bool("server.multiCore"),
		LockOSThread:     globalConfig.Bool("server.lockOSThread"),
		LoadBalancer:     getLoadBalancer(globalConfig.String("server.loadBalancer")),
		ReadBufferCap:    globalConfig.Int("server.readBufferCap"),
		WriteBufferCap:   globalConfig.Int("server.writeBufferCap"),
		SocketRecvBuffer: globalConfig.Int("server.socketRecvBuffer"),
		SocketSendBuffer: globalConfig.Int("server.socketSendBuffer"),
		ReuseAddress:     globalConfig.Bool("server.reuseAddress"),
		ReusePort:        globalConfig.Bool("server.reusePort"),
		TCPKeepAlive:     globalConfig.Duration("server.tcpKeepAlive"),
		TCPNoDelay:       getTCPNoDelay(),
	}
}
