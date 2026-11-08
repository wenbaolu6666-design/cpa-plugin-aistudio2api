package cpa

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/app"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginabi"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type HostCallFunc func(method string, payload []byte) ([]byte, error)

type Plugin struct {
	mu              sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	pluginCfg       PluginConfig
	appCfg          config.Config
	runtime         *app.PluginRuntime
	hostCall        HostCallFunc
	loginStateMu    sync.Mutex
	loginStates     map[string]*loginSessionState
	excludedMu      sync.RWMutex
	accountExcluded map[string][]string
	hostExcluded    map[string][]string
	chromeLogin     sync.Mutex
	serviceStopped  atomic.Bool
	lastRegisterNs  atomic.Int64
}

type loginSessionState struct {
	CreatedAt       time.Time
	InitialAccounts map[string]struct{}
	Provider        string
	Done            bool
	Account         *api.AdminAccount
	ErrorMessage    string
}

func NewPlugin(hostCall HostCallFunc) *Plugin {
	ctx, cancel := context.WithCancel(context.Background())
	return &Plugin{
		ctx:             ctx,
		cancel:          cancel,
		hostCall:        hostCall,
		loginStates:     make(map[string]*loginSessionState),
		accountExcluded: make(map[string][]string),
	}
}

func (p *Plugin) SetHostCall(fn HostCallFunc) {
	p.mu.Lock()
	p.hostCall = fn
	p.mu.Unlock()
}

func (p *Plugin) Runtime() (*app.PluginRuntime, error) {
	p.mu.RLock()
	rt := p.runtime
	p.mu.RUnlock()
	if rt != nil {
		return rt, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runtime != nil {
		return p.runtime, nil
	}
	pcfg, cfg, err := ParsePluginConfig(nil)
	if err != nil {
		return nil, err
	}
	p.pluginCfg = pcfg
	p.appCfg = cfg
	rt, err = app.NewPluginRuntime(p.ctx, cfg)
	if err != nil {
		return nil, err
	}
	p.runtime = rt
	return rt, nil
}

func (p *Plugin) Config() PluginConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pluginCfg
}

type rpcLifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type rpcRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  rpcCapabilities    `json:"capabilities"`
}

type rpcCapabilities struct {
	ModelProvider          bool                         `json:"model_provider"`
	AuthProvider           bool                         `json:"auth_provider"`
	Scheduler              bool                         `json:"scheduler"`
	ModelRouter            bool                         `json:"model_router"`
	Executor               bool                         `json:"executor"`
	ExecutorModelScope     pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats   []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats  []string                     `json:"executor_output_formats,omitempty"`
	RequestLifecyclePlugin bool                         `json:"request_lifecycle_plugin"`
	CommandLinePlugin      bool                         `json:"command_line_plugin"`
	ManagementAPI          bool                         `json:"management_api"`
	QuotaProvider          bool                         `json:"quota_provider"`
}

func (p *Plugin) HandleMethod(method string, raw []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return p.handleRegister(raw)
	case pluginabi.MethodPluginReconfigure, "plugin.reload":
		return p.handleReload(raw)
	case pluginabi.MethodPluginQuiesce:
		return p.handleQuiesce()
	case pluginabi.MethodPluginShutdown:
		return p.handleShutdown()

	// Auth Provider
	case pluginabi.MethodAuthIdentifier:
		return p.handleAuthIdentifier()
	case pluginabi.MethodAuthParse:
		return p.handleAuthParse(raw)
	case pluginabi.MethodAuthLoginStart:
		return p.handleAuthLoginStart(raw)
	case pluginabi.MethodAuthLoginPoll:
		return p.handleAuthLoginPoll(raw)
	case pluginabi.MethodAuthRefresh:
		return p.handleAuthRefresh(raw)

	// Model Provider & Router
	case pluginabi.MethodModelStatic:
		return p.handleModelStatic(raw)
	case pluginabi.MethodModelForAuth:
		return p.handleModelForAuth(raw)
	case pluginabi.MethodModelRoute:
		return p.handleModelRoute(raw)

	// Executor
	case pluginabi.MethodExecutorIdentifier:
		return p.handleExecutorIdentifier()
	case pluginabi.MethodExecutorExecute:
		return p.handleExecutorExecute(raw)
	case pluginabi.MethodExecutorExecuteStream:
		return p.handleExecutorExecuteStream(raw)
	case pluginabi.MethodExecutorCountTokens:
		return p.handleExecutorCountTokens(raw)
	case pluginabi.MethodExecutorHTTPRequest:
		return p.handleExecutorHTTPRequest(raw)

	// Scheduler & Request Lifecycle & Quota
	case pluginabi.MethodSchedulerPick:
		return p.handleSchedulerPick(raw)
	case pluginabi.MethodRequestComplete:
		return p.handleRequestComplete(raw)
	case pluginabi.MethodQuotaIdentifier:
		return p.handleQuotaIdentifier()
	case pluginabi.MethodQuotaDescribe:
		return p.handleQuotaDescribe()
	case pluginabi.MethodQuotaFetch:
		return p.handleQuotaFetch(raw)
	case pluginabi.MethodQuotaReset:
		return p.handleQuotaReset(raw)

	// CLI & Management API
	case pluginabi.MethodCommandLineRegister:
		return p.handleCLIRegister()
	case pluginabi.MethodCommandLineExecute:
		return p.handleCLIRun(raw)
	case pluginabi.MethodManagementRegister:
		return p.handleManagementRegister()
	case pluginabi.MethodManagementHandle:
		return p.handleManagementHandle(raw)

	default:
		return nil, pluginabi.NewError("unsupported_method", fmt.Sprintf("unsupported plugin method %s", method), 501)
	}
}

func (p *Plugin) handleRegister(raw []byte) (any, error) {
	var req rpcLifecycleRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if err := p.applyConfig(req.ConfigYAML); err != nil {
		return nil, err
	}
	return rpcRegistration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginID,
			Version:          PluginVersion,
			Author:           "jianli9158 (Upstream: Mag1cFall)",
			GitHubRepository: "https://github.com/jianli9158/cpa-plugin-aistudio2api",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "waa_backend",
					Type:        pluginapi.ConfigFieldTypeEnum,
					Default:     "spidermonkey",
					EnumValues:  []string{"spidermonkey"},
					Description: "BotGuard WAA 证明运行引擎（进程内 SpiderMonkey VM）",
				},
				{
					Name:        "data_dir",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "~/.aistudio2api",
					Description: "存放 AI Studio 账户凭据、WAA 解释器缓存与模型目录的数据目录",
				},
				{
					Name:        "proxy",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "",
					Description: "可选的 HTTP/SOCKS5 代理地址，用于 Google AI Studio 上游流量",
				},
				{
					Name:        "warm_workers",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     2,
					Description: "常驻就绪的 WAA 预热工作线程数",
				},
				{
					Name:        "max_active_workers",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     5,
					Description: "最大并发活跃 WAA 工作线程数",
				},
				{
					Name:        "per_account_concurrency",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Default:     2,
					Description: "每个 AI Studio 账户的最大并发生成请求数",
				},
				{
					Name:        "route_unprefixed_models",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     true,
					Description: "存在可用账户时自动路由无前缀的 AI Studio 模型（Playground 与 Build 通道）",
				},
				{
					Name:        "sync_auth_files",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     true,
					Description: "自动将导入的 AI Studio 账户凭据同步至 CLIProxyAPI 凭据目录",
				},
				{
					Name:        "fake_stream",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     false,
					Description: "伪流式响应：在上游生成完整响应后，再以流式事件分块推送给客户端",
				},
				{
					Name:        "model_aliases",
					Type:        pluginapi.ConfigFieldTypeString,
					Default:     "",
					Description: "英文逗号分隔的模型别名映射（例如 my-pro:gemini-3.1-pro-preview,my-flash:gemini-3.8-flash）",
				},
			},
		},
		Capabilities: rpcCapabilities{
			ModelProvider:      true,
			AuthProvider:       true,
			Scheduler:          true,
			ModelRouter:        true,
			Executor:           true,
			ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats: []string{
				"chat-completions",
				"responses",
				"anthropic",
				"gemini",
			},
			ExecutorOutputFormats: []string{
				"chat-completions",
				"responses",
				"anthropic",
				"gemini",
			},
			RequestLifecyclePlugin: true,
			CommandLinePlugin:      true,
			ManagementAPI:          true,
			QuotaProvider:          true,
		},
	}, nil
}

func (p *Plugin) handleReload(raw []byte) (any, error) {
	return p.handleRegister(raw)
}

func (p *Plugin) applyConfig(configYAML []byte) error {
	pcfg, cfg, err := ParsePluginConfig(configYAML)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.pluginCfg = pcfg
	p.appCfg = cfg
	p.lastRegisterNs.Store(time.Now().UnixNano())
	p.serviceStopped.Store(false)
	rt := p.runtime
	if rt == nil {
		var rtErr error
		rt, rtErr = app.NewPluginRuntime(p.ctx, cfg)
		if rtErr != nil {
			p.mu.Unlock()
			return rtErr
		}
		p.runtime = rt
	}
	p.mu.Unlock()
	if boolOrDefault(pcfg.SyncAuthFiles, true) && rt != nil {
		p.setHostAuthFilesDisabled(rt, false)
	}
	return nil
}

func (p *Plugin) handleQuiesce() (any, error) {
	p.serviceStopped.Store(true)
	p.mu.RLock()
	rt := p.runtime
	syncAuth := boolOrDefault(p.pluginCfg.SyncAuthFiles, true)
	p.mu.RUnlock()
	if rt != nil {
		_, _ = rt.Admin.StopService(p.ctx)
	}
	if syncAuth && rt != nil {
		p.setHostAuthFilesDisabled(rt, true)
	}
	return struct{}{}, nil
}

func (p *Plugin) handleShutdown() (any, error) {
	p.serviceStopped.Store(true)
	p.mu.Lock()
	rt := p.runtime
	syncAuth := boolOrDefault(p.pluginCfg.SyncAuthFiles, true)
	p.runtime = nil
	cancel := p.cancel
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.mu.Unlock()
	p.loginStateMu.Lock()
	clear(p.loginStates)
	p.loginStateMu.Unlock()
	p.excludedMu.Lock()
	clear(p.accountExcluded)
	clear(p.hostExcluded)
	p.excludedMu.Unlock()
	if syncAuth && rt != nil {
		p.setHostAuthFilesDisabled(rt, true)
	}
	if rt != nil {
		_ = rt.Close()
	}
	if cancel != nil {
		cancel()
	}
	return struct{}{}, nil
}

func (p *Plugin) callHost(method string, req any) (json.RawMessage, error) {
	p.mu.RLock()
	fn := p.hostCall
	p.mu.RUnlock()
	if fn == nil {
		return nil, fmt.Errorf("host callback is unavailable")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	raw, err := fn(method, payload)
	if err != nil {
		return nil, err
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope for %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, env.Error
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return env.Result, nil
}

func (p *Plugin) logHost(callbackID, level, message string, fields map[string]string) {
	req := map[string]any{
		"host_callback_id": strings.TrimSpace(callbackID),
		"level":            level,
		"message":          message,
		"fields":           fields,
	}
	_, _ = p.callHost(pluginabi.MethodHostLog, req)
}
