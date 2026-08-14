// Package acp 实现 zlite 的 ACP（Agent Client Protocol）Agent 端。
//
// 通过 stdio 与 ACP 客户端（编辑器等）通信：每个 ACP session 对应一个
// ~/.zlite/sessions/ 下的 jsonl 会话与一个独立的 agent.Agent 实例，
// agent 事件流经翻译 goroutine 转成 ACP session update 推给客户端。
// 工具全部复用 internal/tools 注册表，零改动。
package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
	"github.com/helloxz/zlite/internal/session"
	"github.com/helloxz/zlite/internal/skills"
	"github.com/helloxz/zlite/internal/tools"
	"github.com/helloxz/zlite/internal/version"
)

// 会话配置选项 ID（client 经 session/set_config_option 切换）。
// mode 选项是 Session Modes（NewSessionResponse.Modes）之外的双通道补充：
// 兼容只实现 session/config 通道的客户端（官方分类 category="mode"）。
// model 选项值格式为 provider_name/model_name（渠道名取自配置 name）。
const (
	configOptionModel    = "model"
	configOptionThinking = "thinking"
	configOptionMode     = "mode"
)

// thinkingEfforts 是可选思考强度列表（与 TUI /thinking 保持一致）。
// auto 表示不传 reasoning_effort 参数，由 API 自行决定。
var thinkingEfforts = []string{"none", "auto", "low", "medium", "high", "xhigh", "max"}

// Options 是 Agent 的构造参数（cmd 入口组装）。
type Options struct {
	Cfg *config.Config
	// ModelName 是初始模型引用（provider_name/model_name），新会话默认使用；
	// 会话恢复时若记录的模型引用可解析则用记录值，否则回退此值。
	ModelName string
	// Cwd 是进程工作目录；client 请求未指定 cwd 时的回退值。
	Cwd string
	Mgr *session.Manager
	// GlobalSkillsDir 是全局 skills 目录（~/.zlite/skills/）；
	// 项目 skills 目录按各会话的 cwd 动态构建。为空时不启用 skills。
	GlobalSkillsDir string
	AutoApprove     bool // 信任模式：跳过权限确认
	// Streamer 覆盖默认模型流（测试注入 fake 用；nil 时按 Cfg 构建）。
	Streamer llm.Streamer
}

// Agent 实现 ACP Agent 接口（acp-go-sdk）。
type Agent struct {
	opts Options
	conn *acpsdk.AgentSideConnection

	mu   sync.Mutex
	sess map[acpsdk.SessionId]*sessionState
}

// sessionState 是一个 ACP 会话的运行时状态：zlite 会话 + 独立 agent 实例 +
// 事件翻译 goroutine 的控制句柄。
type sessionState struct {
	sid   acpsdk.SessionId
	zs    *session.Session // zlite 会话（jsonl 文件句柄）
	ag    *agent.Agent     // 本会话专属 agent（多会话互不干扰）
	model string           // 当前模型名
	cwd   string           // 会话工作目录（client 指定或进程 cwd）

	mu     sync.Mutex
	busy   bool               // 是否有进行中的 turn
	closed bool               // 会话已关闭（CloseSession 已接管），禁止再开始新 turn
	cancel context.CancelFunc // 当前 turn 的取消函数（session/cancel 通知用）

	stop chan struct{} // 关闭事件翻译 goroutine
	wg   sync.WaitGroup
}

// New 创建 ACP Agent（尚未绑定连接，Attach 后可用）。
func New(opts Options) *Agent {
	return &Agent{opts: opts, sess: make(map[acpsdk.SessionId]*sessionState)}
}

// Attach 绑定连接（cmd 入口在 NewAgentSideConnection 后调用）。
func (a *Agent) Attach(conn *acpsdk.AgentSideConnection) {
	a.conn = conn
}

// ---- 连接级方法 ----

// Initialize 返回能力声明：会话方法全量支持（load/list/close/resume），
// modes 与 config options 在 session/new、session/load 响应中返回；
// _meta 携带可用命令（slashCommands，供客户端连接初始化即展示）。
func (a *Agent) Initialize(ctx context.Context, params acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.InitializeResponse{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		AgentInfo: &acpsdk.Implementation{
			Name:    "zlite",
			Version: version.String(),
		},
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: acpsdk.SessionCapabilities{
				List:   &acpsdk.SessionListCapabilities{},
				Close:  &acpsdk.SessionCloseCapabilities{},
				Resume: &acpsdk.SessionResumeCapabilities{},
				Delete: &acpsdk.SessionDeleteCapabilities{},
			},
		},
		AuthMethods: []acpsdk.AuthMethod{},
	}, nil
}

// Authenticate 无需鉴权，直接成功。
func (a *Agent) Authenticate(ctx context.Context, params acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	return acpsdk.AuthenticateResponse{}, nil
}

// Logout 无状态，直接成功。
func (a *Agent) Logout(ctx context.Context, params acpsdk.LogoutRequest) (acpsdk.LogoutResponse, error) {
	return acpsdk.LogoutResponse{}, nil
}

// ---- 会话生命周期 ----

// NewSession 创建会话：新建 zlite 会话（jsonl），ACP session id 复用 zlite id，
// 并记入 jsonl meta 留痕（恢复时按 id 直接查找）。
// 会话 cwd 取 client 请求值（校验为存在的绝对目录）；未指定时回退进程 cwd。
func (a *Agent) NewSession(ctx context.Context, params acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	cwd, err := a.resolveCwd(params.Cwd)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	// 初始渠道名从默认模型引用解析（Create 记录渠道名供会话恢复）
	p, _, err := a.opts.Cfg.ResolveModelSpec(a.opts.ModelName)
	if err != nil {
		return acpsdk.NewSessionResponse{}, fmt.Errorf("初始模型引用无效: %w", err)
	}
	zs, err := a.opts.Mgr.Create(cwd, p.Name, a.opts.ModelName, a.opts.Cfg.Agent.Mode)
	if err != nil {
		return acpsdk.NewSessionResponse{}, err
	}
	_ = zs.AppendMeta("acp_session_id", zs.ID) // 留痕；meta 失败不影响创建

	st := a.newSessionState(zs, a.opts.ModelName, cwd)
	a.addSession(st)
	a.sendAvailableCommands(st.sid)
	return acpsdk.NewSessionResponse{
		SessionId:     st.sid,
		Modes:         modesState(zs.Mode),
		ConfigOptions: a.configOptions(st),
	}, nil
}

// LoadSession 加载已有会话（按 ACP session id = zlite id 查找 jsonl）。
// 会话按 cwd 哈希分区存储，client 须传与创建时一致的 cwd（协议语义），
// 不一致时自然 "session not found"。
// 加载后回放历史消息（agent_message_chunk + agent_thought_chunk），
// 客户端可展示历史对话与思维链。
func (a *Agent) LoadSession(ctx context.Context, params acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	cwd, err := a.resolveCwd(params.Cwd)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	st, err := a.openSession(string(params.SessionId), cwd)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}
	a.sendAvailableCommands(st.sid)
	a.replayHistory(st)
	return acpsdk.LoadSessionResponse{
		Modes:         modesState(st.zs.Mode),
		ConfigOptions: a.configOptions(st),
	}, nil
}

// ResumeSession 恢复会话（与 LoadSession 同实现；不返回历史消息）。
func (a *Agent) ResumeSession(ctx context.Context, params acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	cwd, err := a.resolveCwd(params.Cwd)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	st, err := a.openSession(string(params.SessionId), cwd)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}
	a.sendAvailableCommands(st.sid)
	a.replayHistory(st)
	return acpsdk.ResumeSessionResponse{
		Modes:         modesState(st.zs.Mode),
		ConfigOptions: a.configOptions(st),
	}, nil
}

// ListSessions 列出会话：按请求的 cwd 过滤（协议语义），未指定时列进程 cwd。
// 与 New/Load/Resume 一致要求绝对路径并做 Clean 规范化；
// 目录不存在时返回空列表（查询语义，不报错）。
func (a *Agent) ListSessions(ctx context.Context, params acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	cwd := a.opts.Cwd
	if params.Cwd != nil && *params.Cwd != "" {
		cwd = *params.Cwd
	}
	if !filepath.IsAbs(cwd) {
		return acpsdk.ListSessionsResponse{}, fmt.Errorf("cwd must be an absolute path: %q", cwd)
	}
	cwd = filepath.Clean(cwd)
	infos, err := a.opts.Mgr.List(cwd)
	if err != nil {
		return acpsdk.ListSessionsResponse{}, err
	}
	sessions := make([]acpsdk.SessionInfo, 0, len(infos))
	for _, in := range infos {
		title := in.Title
		updated := in.CreatedAt.Format(time.RFC3339)
		sessions = append(sessions, acpsdk.SessionInfo{
			SessionId: acpsdk.SessionId(in.ID),
			Cwd:       cwd,
			Title:     &title,
			UpdatedAt: &updated,
		})
	}
	return acpsdk.ListSessionsResponse{Sessions: sessions}, nil
}

// closeWaitTimeout 是 CloseSession 等待进行中 turn 结束的超时上限
// （取消后通常立即返回；工具不响应 ctx 取消时兜底）。
// 包级变量便于测试缩短。
var closeWaitTimeout = 5 * time.Second

// CloseSession 关闭会话：取消进行中的 turn、停止事件翻译、关闭 jsonl 文件。
// 置 closed 标志后不再接受新 turn（并发 Prompt 的 begin 会失败），
// 从根上杜绝「清理与 turn 开始」的竞态。
func (a *Agent) CloseSession(ctx context.Context, params acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	st := a.takeSession(params.SessionId)
	if st == nil {
		return acpsdk.CloseSessionResponse{}, nil // 幂等
	}
	a.closeState(st)
	return acpsdk.CloseSessionResponse{}, nil
}

// closeState 关闭会话的运行时状态：取消进行中的 turn、停止事件翻译、
// 关闭 jsonl 文件句柄。调用前须已 takeSession（从管理 map 移除），
// 供 CloseSession 与 UnstableDeleteSession 共用。
func (a *Agent) closeState(st *sessionState) {
	st.markClosed()
	// 等待进行中的 turn 结束（轮询而非忙等锁，避免与 turn 清理互相阻塞）
	deadline := time.Now().Add(closeWaitTimeout)
	for st.isBusy() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(st.stop)
	st.wg.Wait()
	// 仅当 turn 已结束时关闭文件句柄：turn 未结束（超时）时若强制关闭，
	// 后续 agent 继续 Append 会写已关闭/置 nil 的文件句柄（panic 风险），
	// 此时留给进程退出回收（文件内容已实时落盘，无丢失）。
	if !st.isBusy() {
		_ = st.zs.Close()
	}
}

// UnstableDeleteSession 删除会话（experimental session/delete）：
// 关闭运行时状态后异步删除磁盘文件（jsonl + meta 缓存 + 原子写残留）。
// 尽力而为语义：文件删除失败静默（尤其 Windows 上文件句柄占用），
// handler 立即返回成功，不阻塞 client；session 不存在时幂等成功。
// 注意：会话未在本进程打开过时，delete 请求无 cwd 字段，只能按进程 cwd
// 拼路径尝试删除（ACP 与 client 通常同 cwd，命中率高），删不到则静默。
func (a *Agent) UnstableDeleteSession(ctx context.Context, params acpsdk.UnstableDeleteSessionRequest) (acpsdk.UnstableDeleteSessionResponse, error) {
	var path string
	st := a.takeSession(params.SessionId)
	if st != nil {
		// 会话在本进程内：先关闭运行时（取消 turn、等结束、关文件句柄），
		// 否则 Windows 上句柄占用会导致删除失败。
		a.closeState(st)
		path = st.zs.Path
	} else {
		path = a.opts.Mgr.SessionPath(a.opts.Cwd, string(params.SessionId))
		if _, err := os.Stat(path); err != nil {
			return acpsdk.UnstableDeleteSessionResponse{}, nil // 幂等：不存在视为成功
		}
	}
	// 异步删除磁盘文件（jsonl + meta 缓存 + 原子写残留），失败静默。
	go func() {
		os.Remove(path)
		os.Remove(path + ".meta")
		os.Remove(path + ".meta.tmp")
	}()
	return acpsdk.UnstableDeleteSessionResponse{}, nil
}

// Cancel 取消指定会话的进行中 turn（session/cancel 通知）。
func (a *Agent) Cancel(ctx context.Context, params acpsdk.CancelNotification) error {
	if st := a.getSession(params.SessionId); st != nil {
		st.cancelCurrent()
	}
	return nil
}

// ---- 对话 ----

// Prompt 处理一轮对话：提取文本 → 识别 /init 命令 → agent.Run / RunInit。
// 事件翻译 goroutine 负责把 agent 事件流转为 ACP session update。
//
// 并发语义（与 SDK 内建行为对齐）：SDK 在进入本方法前会用 sessionCancels
// 取消同一 session 上一个 prompt 的请求 ctx（最新 prompt 优先），因此这里
// 不拒绝并发，而是等待旧 turn 清理完毕后再开始；若等待期间自身 ctx 被
// 取消（被更新的 prompt 取代），以 cancelled 结束。
func (a *Agent) Prompt(ctx context.Context, params acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	st := a.getSession(params.SessionId)
	if st == nil {
		return acpsdk.PromptResponse{}, fmt.Errorf("session not found: %s", params.SessionId)
	}
	if err := st.waitIdle(ctx); err != nil {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
	}
	if !st.begin() {
		// 等待期间会话被 CloseSession 关闭
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
	}
	defer st.endTurn()

	// 每次 turn 开始时重新通告可用命令（available_commands_update）：
	// 兼容「会话创建时尚未注册通知 handler」的客户端（如 zacp 在首条
	// prompt 前才注册），保证下一次 prompt 必然能捕获命令列表；
	// 与创建/加载会话时的通告形成双保险，幂等无副作用。
	a.sendAvailableCommands(st.sid)

	text := extractPromptText(params.Prompt)
	if strings.TrimSpace(text) == "" {
		return acpsdk.PromptResponse{}, errors.New("prompt must contain text content")
	}

	runCtx, cancel := context.WithCancel(ctx)
	st.setCancel(cancel)
	defer st.setCancel(nil)

	var err error
	if isInitCommand(text) {
		// 与 TUI /init 一致：整条消息（含参数）记入会话，走 init 系统提示词
		err = st.ag.RunInit(runCtx, text)
	} else if isCompressCommand(text) {
		// 与 TUI /compress 一致：压缩全量历史并注入后续上下文（消息不记入会话）
		err = st.ag.Compress(runCtx)
	} else {
		err = st.ag.Run(runCtx, text)
	}
	if err != nil {
		if runCtx.Err() != nil {
			return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
		}
		return acpsdk.PromptResponse{}, err
	}
	return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
}

// ---- 会话配置 ----

// SetSessionMode 切换模式（plan/build），经 agent.SetMode 生效并广播
// current_mode_update（翻译 goroutine 发送）。
// 与 turn 互斥：持 st.mu 完成「busy 检查 + 切换」，避免与并发 Prompt 竞态。
func (a *Agent) SetSessionMode(ctx context.Context, params acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
	st := a.getSession(params.SessionId)
	if st == nil {
		return acpsdk.SetSessionModeResponse{}, fmt.Errorf("session not found: %s", params.SessionId)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.busy {
		return acpsdk.SetSessionModeResponse{}, errors.New("session is busy: cannot switch mode during a turn")
	}
	mode, err := agent.ParseMode(string(params.ModeId))
	if err != nil {
		return acpsdk.SetSessionModeResponse{}, err
	}
	st.ag.SetMode(mode)
	return acpsdk.SetSessionModeResponse{}, nil
}

// SetSessionConfigOption 设置会话配置选项：
//   - model：切换模型（复用 llm.BuildModelNamed + agent.SetStreamer）
//   - thinking：切换思考强度（agent.SetThinking）
//   - mode：切换模式（与 session/set_mode 等效，双通道补充）
//
// 与 turn 互斥：持 st.mu 完成「busy 检查 + 切换」；响应组装（configOptions
// 会再取 st.mu）在释放锁后进行。
func (a *Agent) SetSessionConfigOption(ctx context.Context, params acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	// SessionId 位于变体内部（SetSessionConfigOptionRequest 是 union）
	if params.ValueId == nil && params.Boolean == nil {
		return acpsdk.SetSessionConfigOptionResponse{}, errors.New("invalid config option request")
	}
	var sid acpsdk.SessionId
	switch {
	case params.ValueId != nil:
		sid = params.ValueId.SessionId
	case params.Boolean != nil:
		sid = params.Boolean.SessionId
	}
	st := a.getSession(sid)
	if st == nil {
		return acpsdk.SetSessionConfigOptionResponse{}, fmt.Errorf("session not found: %s", sid)
	}

	applyErr := func() error {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.busy {
			return errors.New("session is busy: cannot change config during a turn")
		}
		switch {
		case params.ValueId != nil:
			switch params.ValueId.ConfigId {
			case configOptionModel:
				spec := string(params.ValueId.Value)
				if _, _, err := a.opts.Cfg.ResolveModelSpec(spec); err != nil {
					return fmt.Errorf("unknown model: %s (%v)", spec, err)
				}
				return st.switchModel(a.opts.Cfg, spec)
			case configOptionThinking:
				name := string(params.ValueId.Value)
				if !slices.Contains(thinkingEfforts, name) {
					return fmt.Errorf("unknown thinking effort: %s (available: %s)", name, strings.Join(thinkingEfforts, ", "))
				}
				st.ag.SetThinking(name)
				return nil
			case configOptionMode:
				mode, err := agent.ParseMode(string(params.ValueId.Value))
				if err != nil {
					return err
				}
				st.ag.SetMode(mode) // 广播 ModeChangeEvent → current_mode_update
				return nil
			default:
				return fmt.Errorf("unknown config option: %s", params.ValueId.ConfigId)
			}
		case params.Boolean != nil:
			return errors.New("boolean config options are not supported")
		default:
			return errors.New("invalid config option request")
		}
	}()
	if applyErr != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, applyErr
	}
	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: a.configOptions(st)}, nil
}

// ---- 内部：会话状态管理 ----

// newSessionState 为会话创建独立 agent 实例（含专属 Approver）。
// model 由调用方决定（新建=初始模型；加载=会话记录的模型，非法时已回退）；
// cwd 为该会话的工作目录（client 指定或进程 cwd）。
func (a *Agent) newSessionState(zs *session.Session, model, cwd string) *sessionState {
	// 构造模型流：测试可经 Options.Streamer 注入 fake；否则按模型引用构建
	var streamer llm.Streamer
	if a.opts.Streamer != nil {
		streamer = a.opts.Streamer
	} else {
		m, err := llm.BuildModelSpec(a.opts.Cfg, model)
		if err != nil {
			// 理论上不会发生：model 均来自配置列表；出错时退回默认模型引用
			m, _ = llm.BuildModelSpec(a.opts.Cfg, a.opts.ModelName)
		}
		streamer = llm.Bind(m)
	}

	// 权限确认：auto_approve 直通；否则经 ACP RequestPermission 交客户端
	var approver agent.Approver
	if a.opts.AutoApprove {
		approver = agent.NewApprover(true)
	} else {
		approver = &acpApprover{conn: a.conn, sid: acpsdk.SessionId(zs.ID)}
	}

	// 工具注册表按会话 cwd 构建：路径解析与命令执行目录跟随会话 cwd
	reg := tools.New(cwd, a.opts.Cfg.Shell.ConfirmCommands, a.opts.Cfg.Shell.PlanExtraCommands)
	// skills 按会话 cwd 构建：项目 skills 目录 <cwd>/.zlite/skills/，
	// 全局目录不变；GlobalSkillsDir 为空时不启用 skills
	var skm *skills.Manager
	if a.opts.GlobalSkillsDir != "" {
		skm = skills.New(a.opts.GlobalSkillsDir, filepath.Join(cwd, ".zlite", "skills"))
		reg.Register(tools.ReadSkillTool(skm))
	}
	// skills：nil 时安全传 nil 接口（避免 nil *Manager 装箱为非 nil 接口）
	var sk agent.SkillsProvider
	if skm != nil {
		sk = skm
	}
	ag := agent.New(a.opts.Cfg, streamer, reg, zs, approver, cwd, agent.Mode(zs.Mode), sk)
	st := &sessionState{
		sid:   acpsdk.SessionId(zs.ID),
		zs:    zs,
		ag:    ag,
		model: model,
		cwd:   cwd,
		stop:  make(chan struct{}),
	}
	a.startPump(st)
	return st
}

// openSession 打开或复用会话（LoadSession/ResumeSession 共用）。
// cwd 为请求解析后的会话目录（客户端须与创建时一致）。
func (a *Agent) openSession(id, cwd string) (*sessionState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.sess[acpsdk.SessionId(id)]; ok {
		// 内存中已打开的会话同样校验 cwd（客户端须用一致的 cwd）
		if st.cwd != cwd {
			return nil, fmt.Errorf("session not found: %s (cwd mismatch)", id)
		}
		return st, nil
	}
	zs, err := a.opts.Mgr.Open(cwd, id)
	if err != nil {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	// 会话记录的模型引用可解析则恢复（新格式或旧格式兼容，见
	// config.RestoreModelSpec；配置变更后可能失效），否则回退默认模型。
	model := a.opts.ModelName
	if spec, err := a.opts.Cfg.RestoreModelSpec(zs.Provider, zs.Model); err == nil {
		model = spec
	}
	st := a.newSessionState(zs, model, cwd)
	a.sess[st.sid] = st
	return st, nil
}

// resolveCwd 解析 client 请求中的 cwd：空值回退进程 cwd；
// 必须是存在的绝对目录路径，否则报错（NewSession/Load/Resume 用）。
// 返回前做 filepath.Clean 规范化（消除 ".."/尾斜杠/重复分隔符导致的
// 分区哈希不一致与 cwd 比对误报）。
func (a *Agent) resolveCwd(reqCwd string) (string, error) {
	cwd := reqCwd
	if cwd == "" {
		cwd = a.opts.Cwd
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd must be an absolute path: %q", cwd)
	}
	st, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd does not exist: %q", cwd)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("cwd is not a directory: %q", cwd)
	}
	return filepath.Clean(cwd), nil
}

func (a *Agent) addSession(st *sessionState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sess[st.sid] = st
}

func (a *Agent) getSession(id acpsdk.SessionId) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess[id]
}

func (a *Agent) takeSession(id acpsdk.SessionId) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sess[id]
	delete(a.sess, id)
	return st
}

// switchModel 切换会话模型：按模型引用 "provider_name/model_name" 重建
// streamer 并注入 agent（TUI /switch 同语义）。
func (s *sessionState) switchModel(cfg *config.Config, spec string) error {
	m, err := llm.BuildModelSpec(cfg, spec)
	if err != nil {
		return err
	}
	s.ag.SetStreamer(llm.Bind(m))
	s.model = spec
	_ = s.zs.AppendMeta("model_change", spec) // 落盘留痕；meta 失败不影响切换
	return nil
}

// ---- 内部：busy / cancel ----

// waitIdle 等待进行中的 turn 结束（SDK 已在进入 Prompt 前取消旧 turn 的
// 请求 ctx，旧 turn 即将清理）；自身 ctx 被取消时返回错误。
func (s *sessionState) waitIdle(ctx context.Context) error {
	for {
		s.mu.Lock()
		idle := !s.busy
		s.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// begin 标记 turn 开始（调用前须已 waitIdle）。
// 返回 false 表示会话已被 CloseSession 关闭，不允许再开始新 turn
// （关闭清理与 turn 开始之间的竞态防护）。
func (s *sessionState) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.busy = true
	return true
}

// markClosed 标记会话已关闭并取消进行中的 turn（CloseSession 调用）。
func (s *sessionState) markClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *sessionState) endTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	s.cancel = nil
}

func (s *sessionState) setCancel(c context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = c
}

func (s *sessionState) cancelCurrent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *sessionState) isBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// ---- 内部：能力声明 ----

// modesState 返回会话模式声明（plan/build，与 agent.Mode 一致）。
func modesState(current string) *acpsdk.SessionModeState {
	planDesc := "Read-only mode: file modifications and shell writes are rejected"
	buildDesc := "Full mode: can modify files and run commands (dangerous commands require approval)"
	return &acpsdk.SessionModeState{
		AvailableModes: []acpsdk.SessionMode{
			{Id: "plan", Name: "Plan", Description: &planDesc},
			{Id: "build", Name: "Build", Description: &buildDesc},
		},
		CurrentModeId: acpsdk.SessionModeId(current),
	}
}

// configOptions 返回会话配置选项：model / thinking / mode 三个下拉选项，
// client 经 session/set_config_option 切换。
// mode 选项是 Session Modes 通道（NewSessionResponse.Modes）之外的双通道补充，
// 顺序追加在末尾（model、thinking 之后），避免影响按索引读取的既有客户端。
func (a *Agent) configOptions(st *sessionState) []acpsdk.SessionConfigOption {
	modelOpts := make([]acpsdk.SessionConfigSelectOption, 0, len(a.opts.Cfg.Providers))
	for _, m := range a.opts.Cfg.AllModels() {
		modelOpts = append(modelOpts, acpsdk.SessionConfigSelectOption{Name: m, Value: acpsdk.SessionConfigValueId(m)})
	}
	thinkingOpts := make([]acpsdk.SessionConfigSelectOption, 0, len(thinkingEfforts))
	for _, t := range thinkingEfforts {
		thinkingOpts = append(thinkingOpts, acpsdk.SessionConfigSelectOption{Name: t, Value: acpsdk.SessionConfigValueId(t)})
	}
	modeOpts := []acpsdk.SessionConfigSelectOption{
		{Name: "plan", Value: "plan"},
		{Name: "build", Value: "build"},
	}

	catModel := acpsdk.SessionConfigOptionCategoryModel
	catThought := acpsdk.SessionConfigOptionCategoryThoughtLevel
	catMode := acpsdk.SessionConfigOptionCategoryMode
	modelSel := acpsdk.SessionConfigSelectOptionsUngrouped(modelOpts)
	thinkingSel := acpsdk.SessionConfigSelectOptionsUngrouped(thinkingOpts)
	modeSel := acpsdk.SessionConfigSelectOptionsUngrouped(modeOpts)
	// 当前值可能被并发的 set_config_option 修改，锁内读取
	st.mu.Lock()
	currentModel := st.model
	currentThinking := st.ag.Thinking()
	currentMode := st.ag.Mode()
	st.mu.Unlock()
	return []acpsdk.SessionConfigOption{
		{Select: &acpsdk.SessionConfigOptionSelect{
			Id:           configOptionModel,
			Name:         "Model",
			Type:         "select",
			Category:     &catModel,
			Description:  acpsdk.Ptr("The model used for this session"),
			CurrentValue: acpsdk.SessionConfigValueId(currentModel),
			Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &modelSel},
		}},
		{Select: &acpsdk.SessionConfigOptionSelect{
			Id:           configOptionThinking,
			Name:         "Thinking effort",
			Type:         "select",
			Category:     &catThought,
			Description:  acpsdk.Ptr("Reasoning effort for the model (auto = let the API decide)"),
			CurrentValue: acpsdk.SessionConfigValueId(currentThinking),
			Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &thinkingSel},
		}},
		{Select: &acpsdk.SessionConfigOptionSelect{
			Id:           configOptionMode,
			Name:         "Mode",
			Type:         "select",
			Category:     &catMode,
			Description:  acpsdk.Ptr("Agent mode: plan (read-only) or build (full, with approval)"),
			CurrentValue: acpsdk.SessionConfigValueId(currentMode),
			Options:      acpsdk.SessionConfigSelectOptions{Ungrouped: &modeSel},
		}},
	}
}

// sendAvailableCommands 宣告可用 /命令：ACP 模式下仅 /init 与 /compress
// （命令名不带斜杠，斜杠由 client 展示层添加）。
func (a *Agent) sendAvailableCommands(sid acpsdk.SessionId) {
	if a.conn == nil {
		return
	}
	_ = a.conn.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		SessionId: sid,
		Update: acpsdk.SessionUpdate{AvailableCommandsUpdate: &acpsdk.SessionAvailableCommandsUpdate{
			AvailableCommands: []acpsdk.AvailableCommand{
				{
					Name:        "init",
					Description: "Analyze the project and generate/refresh AGENTS.md",
					Input: &acpsdk.AvailableCommandInput{Unstructured: &acpsdk.UnstructuredCommandInput{
						Hint: "Optional extra requirements for the init task",
					}},
				},
				{
					Name:        "compress",
					Description: "Summarize the full conversation once and inject it as context",
				},
			},
			SessionUpdate: "available_commands_update",
		}},
	})
}

// replayHistory 在会话加载/恢复时把历史 assistant 消息回放为 session/update
// 通知（agent_message_chunk + agent_thought_chunk），客户端可展示历史对话
// 与思维链（与 reasonix 等 agent 行为一致，zacp 的 mutedSessions 机制即为
// 此类回放设计）。reasoning 已落盘于 jsonl，回放不会进入模型上下文。
func (a *Agent) replayHistory(st *sessionState) {
	if a.conn == nil {
		return
	}
	ctx := context.Background()
	for _, r := range st.zs.History {
		if r.Type != session.TypeMessage || r.Role != "assistant" {
			continue
		}
		if r.Content != "" {
			_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
				SessionId: st.sid,
				Update:    acpsdk.UpdateAgentMessageText(r.Content),
			})
		}
		if r.Reasoning != "" {
			_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
				SessionId: st.sid,
				Update:    acpsdk.UpdateAgentThoughtText(r.Reasoning),
			})
		}
	}
}

// ---- 内部：消息处理 ----

// extractPromptText 提取 prompt 中的文本内容（ContentBlock::Text 拼接）。
// 非文本块（image/audio/resource 等）本期不支持，静默忽略。
func extractPromptText(blocks []acpsdk.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text != nil {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text.Text)
		}
	}
	return b.String()
}

// isInitCommand 判断用户消息是否为 /init 命令（"/init" 或 "/init <要求>"）。
func isInitCommand(text string) bool {
	t := strings.TrimSpace(text)
	return t == "/init" || strings.HasPrefix(t, "/init ")
}

// isCompressCommand 判断用户消息是否为 /compress 命令（无参数；带参数的
// 写法与 TUI 一致：触发压缩并忽略参数）。
func isCompressCommand(text string) bool {
	t := strings.TrimSpace(text)
	return t == "/compress" || strings.HasPrefix(t, "/compress ")
}
