package app

// stale_read_optin.go — 「写后回读」在 STALE_READ 机械门上的**精确放行位**。
//
// 背景:daemon 侧 internal/daemon/stalereads.go 把铁律 5(PCB mutation 之后、
// `doc reload` 之前不许读 PCB)从劝告升成了**拒绝**:任何 pcb.* 读动作,只要目标
// 窗口自上次 reload 后发生过 pcb.* 写,就直接回 STALE_READ。
//
// 但 internal/app 里有一整类命令,**正常工作方式就是「写 → 立刻回读验证」**:
//
//	pcb refine            每步 modify 之后重新拉快照 + 数 check finding,
//	                      读不到就保守回滚 —— 被拦死等于整条命令退化成必然失败的 no-op
//	pcb sync-designators  写完位号回读 components.list,「只有回读一致才算修好」
//	pcb power-planes      内电层配方:pour → 改层类型 → 读回层/线,再 rebuild
//	pcb power-pour        第二条电源轨之前要读上一条铺出来的 pour
//	pcb autoroute         收尾 DRC
//	easyeda apply         playbook 是从真实会话录出来的,mutate 步后面就跟 verify 块
//
// 这些回读**不是**铁律 5 要防的那种「拿旧状态当真相」——它们读的正是刚写的那批
// 图元,且判据本身就是「读回来和写下去的一致吗」。所以需要一个放行位。
//
// ── 为什么不是又一个全局开关 ───────────────────────────────────────────
//
// 现成的 `cfg.forceReason` 有两条硬伤,新开一个 `cfg.staleReadReason` 会原样继承
// 第一条:
//
//  1. **整进程生效**。appConfig 是所有 208 处 dispatch 共用的那一份;为了一次回读
//     把它打开,等于把这条命令**余下全部** PCB 读都从门里放出去 —— 包括那些真该被
//     拦的(例如 refine 下一轮开头本该看到新状态的规划读)。放行位必须窄到「这一次
//     调用」,而不是「这条命令」。
//  2. **语义已被占用**:daemon 侧 forceReason 同时是布线阶段门(stagegate.go
//     CheckRouteGate)的越权钥匙。拿它放行回读 = 顺手解锁布线门,安全倒退。
//
// ── 这里的做法 ─────────────────────────────────────────────────────────
//
// 放行位是**一次调用上的值**,不是配置里的位:staleReadOptIn 返回 cfg 的一份
// **作用域副本**,只喂给紧接着的那一次 dispatch(requestReadAfterWrite 把这两步
// 焊死成一个调用,让人没机会把副本存起来复用)。
//
// 硬伤 2 用**机械收窄**堵死,而不是靠调用方自觉:副本里的理由只有在**请求**
// (动作 + 载荷)满足下面两条之一时才会被写到线上(staleReadEligibleRequest):
//
//	Domain == pcb  &&  Mutates == false  &&  RequiresGate == ""           ← 普通读
//	Domain == pcb  &&  RequiresGate == ""  &&  载荷里 dryRun == true       ← 预览读
//
// 两条都直接从 protocol.AllActions() 目录生成。第二条不是补丁,是**对齐 daemon 的
// 同一个谓词**:daemon 判读/写用的是「目录 Mutates 减去 dry-run 预览」,所以
// `pcb.page.clear --dry-run` 会被门当读拦下 —— app 侧若只按动作名判,就会出现
// 「拦得住却放不开」的洞(`pcb clear` 的 #121 收尾计数正卡在这个洞里)。
//
// 无论走哪一条,RequiresGate == "" 都不放松。于是即便有人把副本误传进一个会写画布
// 的 helper,它也**不可能**解锁布线门 —— 那些动作要么带 gate="routing",要么根本
// 不接受 dryRun。
//
// 审计:daemon 收到带 forceReason 的读会自己写一行 `daemon.stale_read.force`
// 伪动作(stalereads.go checkStaleRead)。所以放行**自动入审计**,app 侧不需要、
// 也不应该另造格式。理由串前缀 "写后回读" 让审计里一眼分得清:这是回读放行,
// 不是人在敲 --force-reason 硬闯。
//
// 什么时候**不该**用它:读的目标不是本命令刚写的那批图元(例如重新规划、重新
// 评分整块板),那种情况该老老实实 `doc reload`,或者把 reload 编进配方。
// 实例:`pcb refine` 环的入口快照、`route-critical` 的叠层入口读 —— 两处都**不**
// 放行,后者读不到时宁可打一行「按 2 层降级」的警告,也不把命令入口的规划读
// 伪装成写后回读。
//
// 唯一的例外形态是 setDispatchStaleReadReason(本文件下半部分):放行位要穿过一次
// **进程内 CLI 再入**(apply playbook 的 `run:` verify 块)时,作用域副本传不过去,
// 只能用 defer 框住的作用域全局。它仍然要过上面那张收窄表。

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/easyeda-agent/internal/protocol"
)

// staleReadCode 是 daemon 拒绝一次陈旧 PCB 读时用的错误码(internal/daemon/
// stalereads.go)。app 侧靠它把「门拦下的」和「真的读失败了」分开 —— 两者的下一步
// 完全不同(前者 reload,后者查连接器),混成一句 "read failed" 就等于没诊断。
const staleReadCode = "STALE_READ"

// errStaleRead 是可用 errors.Is 匹配的哨兵,配合 actionError.Is 使用。
var errStaleRead = errors.New(staleReadCode)

// actionError 是一次 /action 返回 ok=false 的结构化形态。**Error() 的文本与旧版
// 逐字一致**(旧版是 fmt.Errorf("%s failed: %s")),所以既有的报文、测试、日志都
// 不受影响;新增的只是可编程判别的 Code。
type actionError struct {
	Action  string
	Code    string
	Message string
}

func (e *actionError) Error() string {
	return fmt.Sprintf("%s failed: %s", e.Action, e.Message)
}

// Is 让 errors.Is(err, errStaleRead) 成立 —— 判据是错误码,不是文本匹配。
func (e *actionError) Is(target error) bool {
	return target == errStaleRead && e.Code == staleReadCode
}

// isStaleRead 报告 err 是不是 STALE_READ 门拦下来的。包成函数是因为调用点很多,
// 而「怎么判 STALE_READ」这件事只该有一个答案。
func isStaleRead(err error) bool { return errors.Is(err, errStaleRead) }

// staleReadNextStep 是给用户看的下一步。daemon 的 message 里已经带了具体命令,
// 这里只在 app 侧补一句「这条命令为什么会撞上」,不重复 daemon 的正文。
func staleReadNextStep(what string) string {
	return fmt.Sprintf("%s 撞上 STALE_READ 机械门(铁律 5):PCB 自上次写后没 reload。"+
		"若这确实是写后回读,请给这处回读加放行位(staleReadOptIn);"+
		"若是重新规划,先 `easyeda doc reload` 再重跑", what)
}

// staleReadEligible 是**允许**携带回读放行位的动作白名单:PCB 域、不改画布、且
// 不受布线阶段门管辖。目录驱动(protocol.AllActions()),所以新增动作自动归位,
// 不会因为有人忘了改这张表而悄悄放宽或收紧。
//
// 三个条件缺一不可:
//   - Domain == pcb —— 只有 PCB 读会被 STALE_READ 门拦,别的域给了也没用,只会
//     在审计里留下误导性的 force 记录。
//   - !Mutates —— 放行位是给**读**的。给写动作带 forceReason 才是真正的危险面。
//   - RequiresGate == "" —— 这是堵死「顺手解锁布线门」的那道机械收窄。
var staleReadEligible = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range protocol.AllActions() {
		if a.Domain == protocol.DomainPcb && !a.Mutates && a.RequiresGate == "" {
			m[a.Name] = true
		}
	}
	return m
}()

// staleReadOptIn 返回 cfg 的**作用域副本**,带上一次性的写后回读放行理由。
//
// 返回副本而不是就地改 cfg 是这套设计的全部要害:调用方拿到的东西**没有**回指
// 共享配置的通道,所以放行不会外溢到本命令的其他读。
//
// reason 为空时原样返回 cfg(不放行)——让「有没有理由」成为放行的唯一开关,
// 避免出现「打开了但没写理由」的半吊子状态。
func staleReadOptIn(cfg *appConfig, reason string) *appConfig {
	r := strings.TrimSpace(reason)
	if cfg == nil || r == "" {
		return cfg
	}
	c := *cfg
	c.staleReadReason = r
	return &c
}

// staleReadDryRunEligible 是**第二类**可携带放行位的动作:目录标着 Mutates=true,
// 但请求本身带 `dryRun:true`,于是 daemon 把它当成一次**读**。
//
// 这条不是补丁,是对齐 daemon 的同一个谓词。daemon 的 requestMutates =
// 「目录 Mutates 减去 dryRun 预览」(internal/daemon/autosave.go),而 STALE_READ
// 门用的正是这个谓词 —— 所以 `pcb.page.clear --dry-run` 会被门当读拦下。app 侧若
// 只按动作名判(!Mutates),放行位就**装不上**这类请求:被拦是按请求判的,放行却是
// 按动作名判的,两把尺不一致 = 一定有拦得住却放不开的洞。
// `pcb clear` 的收尾「还剩多少」计数就在这个洞里(runPcbClearVerified:
// pass2 clear 之后那次 dry-run 计数)。
//
// 仍然目录驱动:哪些动作**支持** dryRun 直接从 Inputs 声明里读("dryRun optional …"),
// 所以新增一个可 dry-run 的动作自动归位。三个收窄条件一个不放:PCB 域、
// 无阶段门(RequiresGate == "",堵死「顺手解锁布线门」)、且必须真的带 dryRun:true
// 才生效(见 staleReadEligibleRequest)—— 一个 dryRun:true 的请求按定义不落画布。
var staleReadDryRunEligible = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range protocol.AllActions() {
		if a.Domain != protocol.DomainPcb || !a.Mutates || a.RequiresGate != "" {
			continue
		}
		for _, in := range a.Inputs {
			if strings.HasPrefix(strings.TrimSpace(in), "dryRun") {
				m[a.Name] = true
				break
			}
		}
	}
	return m
}()

// staleReadEligibleRequest 判一次**请求**(动作 + 载荷)能不能带放行位。
// 与 daemon 的 pcbStaleRead/requestMutates 逐条对应,所以「拦得住的都放得开」。
func staleReadEligibleRequest(action string, payload any) bool {
	if staleReadEligible[action] {
		return true
	}
	if !staleReadDryRunEligible[action] {
		return false
	}
	m, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	b, _ := m["dryRun"].(bool)
	return b
}

// ── 跨 CLI 再入的放行位(playbook verify 专用)────────────────────────────
//
// `easyeda apply` 的 verify 块可以是 `run: pcb layout-lint` 这种**子命令**,而
// runSubcommand 是把 argv 交给 Run() 在进程内重新走一遍根命令 —— 那边会**新建**
// 一份 appConfig,作用域副本传不过去。
//
// 所以这里留一个带 defer 还原的**作用域全局**,和 setDispatchDryRun 同一套路(它
// 存在的理由一模一样:dry-run 纯度也要穿过同一个进程内再入)。它比 cfg.forceReason
// 窄两级:(1) 生存期由 defer 严格框在 verify 块那几行里,不是整条命令;(2) 仍然
// 要过 staleReadEligibleRequest 那张表,所以照样解锁不了布线门。
//
// 只有「一次调用装不下」的场景才该用它。普通写后回读一律用 staleReadOptIn。
var (
	dispatchStaleReadMu     sync.Mutex
	dispatchStaleReadReason string
)

// setDispatchStaleReadReason 打开作用域放行位,返回还原函数(照 setDispatchDryRun
// 的形状,调用点一律 `defer setDispatchStaleReadReason(...)()`)。
func setDispatchStaleReadReason(reason string) (restore func()) {
	dispatchStaleReadMu.Lock()
	prev := dispatchStaleReadReason
	dispatchStaleReadReason = strings.TrimSpace(reason)
	dispatchStaleReadMu.Unlock()
	return func() {
		dispatchStaleReadMu.Lock()
		dispatchStaleReadReason = prev
		dispatchStaleReadMu.Unlock()
	}
}

func activeDispatchStaleReadReason() string {
	dispatchStaleReadMu.Lock()
	defer dispatchStaleReadMu.Unlock()
	return dispatchStaleReadReason
}

// staleReadForceReason 给出本次请求要写到线上的 forceReason(空 = 不放行)。
// postAction 在派发咽喉上调用它,所以收窄规则只有一份实现。
func staleReadForceReason(cfg *appConfig, action string, payload any) string {
	reason := ""
	if cfg != nil {
		reason = cfg.staleReadReason
	}
	if reason == "" {
		reason = activeDispatchStaleReadReason()
	}
	if reason == "" || !staleReadEligibleRequest(action, payload) {
		return ""
	}
	return "写后回读: " + reason
}

// requestReadAfterWrite 是给「写完立刻回读」用的唯一入口:一次调用 = 一次放行。
//
// 把「造副本」和「发请求」焊成一个函数,是为了让副本没有生存期 —— 没有变量能
// 捧着它去做第二次读,也就没有「放行位悄悄扩大」的路径。
func requestReadAfterWrite(cfg *appConfig, action, window string, payload any, reason string) (*actionResult, error) {
	return requestAction(staleReadOptIn(cfg, reason), action, window, payload)
}

// requestReadAfterWriteTimed 同上,但可指定往返预算 —— 收尾 DRC 这类重读动作
// 在真板上例行超过默认窗口。
func requestReadAfterWriteTimed(cfg *appConfig, action, window string, payload any, reason string, timeout time.Duration) (*actionResult, error) {
	return requestActionTimed(staleReadOptIn(cfg, reason), action, window, payload, timeout)
}
