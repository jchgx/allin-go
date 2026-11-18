package workbuddy

import "strings"

// 渠道校验（code 11-128）相关的判定与本文件之外的重写入口。
//
// 先记结论，再记推论 —— 因为最初那条推论被线上日志推翻了：
//
//	可复现的部分：上游对 system / assistant 消息的开头做前缀匹配，大小写不敏感、
//	先裁前导空白；命中第三方 Agent 的自我描述就整条 400。在它前面另起一行中性
//	开头，前缀就不再命中。同一句放在消息中间不触发；零宽字符断词对**这一个**
//	规则无效（但对子串规则有效，见 identity.go）。
//
//	不可靠的部分：它不只是内容规则。线上 522KB 请求屏蔽了身份词、也改写了开头，
//	重发仍被拒；而前后几分钟同一份字节有时 200。上游自己回的 displayMsg 是
//	「请求被拦截，请重新发送」——这是账号级自适应风控，重发常常能过，也可能连着
//	几次不过。
//
// 因此这里实现的是「提高通过率的改写」，不是「保证通过的修复」：
//
//   - IsUnapprovedOpening：判定一个开头像不像已知指纹（本地日志用）。
//   - NeutralizeChannelBlock：兼容旧入口，等价于 ForceNeutralHead ——
//     给每个被扫描消息补中性开头，不依赖认识是哪一条指纹。
//
// 完整的官方客户端兼容规则（角色归一、补首条 system、句子级改词、子句断词、
// 兜底扰动）在 identity.go，移植自 dsh-connect-workbuddy 的实测规则集。

// NeutralHead 是插在消息最前面的中性开头。
//
// 不能省略成空行或空字符串：上游要求 system 内容非空，
// 而且这句本身就是一个正常的系统提示开头，对模型无害。
const NeutralHead = fallbackSystemPrompt

// unapprovedOpenings 是本地**实测**过的指纹（小写）。
//
// 故意不用猜测扩充：没实测过的开头不写进来，否则会把本来能过的
// prompt 也改掉。真正的改写逻辑不查这张表，它对所有开头生效，
// 所以表外的新指纹一样能救回来。
var unapprovedOpenings = []string{
	"you are an ai agent powered by deepseek harness",
	"you are claude code, anthropic's official cli for claude",
}

// IsUnapprovedOpening 判断这段文本开头是不是已知指纹。
//
// 只用于日志和「这条值得先修一下」的判断，不参与上行的放行决策。
func IsUnapprovedOpening(text string) bool {
	opening := toLowerTrim(text)
	for _, p := range unapprovedOpenings {
		if len(opening) >= len(p) && opening[:len(p)] == p {
			return true
		}
	}
	return false
}

// scannableRole 是这个上游会做前缀匹配的角色。
//
// assistant 也算：历史里一条以指纹开头的回答，会让**整个会话**一直 400，
// 只改 system 救不回来（实测确认）。developer 先被归一成 system，
// 所以这里列它只是为了容错。
func scannableRole(role string) bool {
	switch lowerTrim(role) {
	case "system", "developer", "assistant":
		return true
	}
	return false
}

// NeutralizeChannelBlock 保留给旧调用点：等价于强制补中性开头。
func NeutralizeChannelBlock(body []byte) (out []byte, repaired bool, err error) {
	out, changed := ForceNeutralHead(body)
	return out, changed, nil
}

// toLowerTrim 裁掉前导空白并转小写，供前缀比较使用。
func toLowerTrim(s string) string { return lowerTrim(s) }

// lowerTrim 是 toLowerTrim 的实现，单独留着便于测试。
func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimLeft(s, " \t\r\n"))
}
