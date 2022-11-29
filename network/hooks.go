package network

import (
	"fmt"
	"sort"

	"github.com/rs/zerolog"
)

type (
	Prio     uint
	HookType string
)

type Signature map[string]interface{}

type HookDef func(Signature) Signature

type Policy int

const (
	Ignore Policy = iota // Ignore errors and continue
	Abort                // Abort on first error and return results
	Remove               // Remove the hook from the list on error and continue
)

const (
	OnConfigLoaded    HookType = "onConfigLoaded"
	OnNewLogger       HookType = "onNewLogger"
	OnNewPool         HookType = "onNewPool"
	OnNewProxy        HookType = "onNewProxy"
	OnNewServer       HookType = "onNewServer"
	OnSignal          HookType = "onSignal"
	OnRun             HookType = "onRun"
	OnBooting         HookType = "onBooting"
	OnBooted          HookType = "onBooted"
	OnOpening         HookType = "onOpening"
	OnOpened          HookType = "onOpened"
	OnClosing         HookType = "onClosing"
	OnClosed          HookType = "onClosed"
	OnTraffic         HookType = "onTraffic"
	OnIncomingTraffic HookType = "onIncomingTraffic"
	OnOutgoingTraffic HookType = "onOutgoingTraffic"
	OnShutdown        HookType = "onShutdown"
	OnTick            HookType = "onTick"
	OnNewClient       HookType = "onNewClient"
)

type HookConfig struct {
	hooks        map[HookType]map[Prio]HookDef
	Logger       zerolog.Logger
	Verification Policy
}

func NewHookConfig() *HookConfig {
	return &HookConfig{
		hooks: make(map[HookType]map[Prio]HookDef),
	}
}

func (h *HookConfig) AddHook(hookType HookType, prio Prio, hook interface{}) {
	if hookDef, ok := hook.(HookDef); ok {
		h.hooks[hookType][prio] = hookDef
	}
}

func (h *HookConfig) GetHook(hookType HookType) map[Prio]HookDef {
	return h.hooks[hookType]
}

func verify(params, returnVal Signature) bool {
	for key := range returnVal {
		if _, ok := params[key]; !ok {
			return false
		}
	}

	return true
}

//nolint:funlen
func (h *HookConfig) RunHooks(
	hookType HookType, args Signature, verification Policy,
) Signature {
	// Sort hooks by priority
	priorities := make([]Prio, 0, len(h.hooks[hookType]))
	for prio := range h.hooks[hookType] {
		priorities = append(priorities, prio)
	}
	sort.SliceStable(priorities, func(i, j int) bool {
		return priorities[i] < priorities[j]
	})

	// Run hooks, passing the result of the previous hook to the next one
	returnVal := make(Signature)
	var removeList []Prio
	// The signature of parameters and args MUST be the same for this to work
	for idx, prio := range priorities {
		var result Signature
		if idx == 0 {
			result = h.hooks[hookType][prio](args)
		} else {
			result = h.hooks[hookType][prio](returnVal)
		}

		// This is done to ensure that the return value of the hook is always valid,
		// and that the hook does not return any unexpected values.
		if verify(args, result) {
			// Update the last return value with the current result
			returnVal = result
			continue
		}

		// At this point, the hook returned an invalid value, so we need to handle it.
		// The result of the current hook will be ignored, regardless of the policy.
		switch verification {
		case Ignore:
			errMsg := fmt.Sprintf(
				"Hook %s (Prio %d) returned invalid value, ignoring", hookType, prio)
			// Logger is not available when loading configuration, so we can't log anything
			if hookType != OnConfigLoaded {
				h.Logger.Error().Msgf(errMsg)
			} else {
				panic(errMsg)
			}
			if idx == 0 {
				returnVal = args
			}
			continue
		case Abort:
			errMsg := fmt.Sprintf(
				"Hook %s (Prio %d) returned invalid value, aborting", hookType, prio)
			if hookType != OnConfigLoaded {
				h.Logger.Error().Msgf(errMsg)
			} else {
				panic(errMsg)
			}
			if idx == 0 {
				return args
			}
			return returnVal
		case Remove:
			errMsg := fmt.Sprintf(
				"Hook %s (Prio %d) returned invalid value, removing", hookType, prio)
			if hookType != OnConfigLoaded {
				h.Logger.Error().Msgf(errMsg)
			} else {
				panic(errMsg)
			}
			removeList = append(removeList, prio)
			if idx == 0 {
				returnVal = args
			}
			continue
		}
	}

	// Remove hooks that failed verification
	for _, prio := range removeList {
		delete(h.hooks[hookType], prio)
	}

	return returnVal
}
