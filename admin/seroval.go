package admin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ----------------------------------------------------------------
// seroval 协议常量 — 升级协议时集中改这里
//
// 来源：opencode.ai 前端 bundle (版本追踪: opencode.ai/go 页面中 /assets/index-*.js)
// 通过浏览器开发者工具抓包 + Bundle 反汇编确认。
// 这些值与当前线上协议版本绑定，升级协议（如 flush serialization 参数变化）
// 时需要同步更新。
// ----------------------------------------------------------------

const (
	// serovalFeatureFlags 是 Mo() serialization 的 features flag。
	// 当前值 63 = 0b111111（所有 6 个特性位全开）。
	// 来源：2026-07 前端 bundle index-3a4c996a.js 中 mo({features:X}) 实参。
	// 历史值：31（2026-06），对应 5 个特性位。
	serovalFeatureFlags = 63

	// serovalStreamPrefix 是 seroval streaming 响应的固定前缀
	serovalStreamPrefix = ";0x"
)

// seroval 序列化类型标签（Mo() protocol types）
// 来源：前端 bundle 中 typeDec 映射表：
//
//	typeDec = {
//	  1: "string",
//	  9: "array",
//	 10: "object",
//	}
//
// 这些是 seroval Mo() API 内部使用的 wire type codes，
// 并非 ECMA 标准，系 seroval 私有协议。
const (
	serovalTypeString = 1  // typeDec[1] → "string"
	serovalTypeArray  = 9  // typeDec[9] → "array"
	serovalTypeObject = 10 // typeDec[10] → "object"
)

// ----------------------------------------------------------------
// 第 0 层：请求编码器
// ----------------------------------------------------------------

// serovalRequestBody 构建 Mo()-compatible POST body。
func serovalRequestBody(items []string) ([]byte, error) {
	payload := serovalBuildArray(items)
	return json.Marshal(map[string]any{
		"t": payload,
		"f": serovalFeatureFlags,
		"m": []any{},
	})
}

func serovalBuildArray(items []string) map[string]any {
	elems := make([]map[string]any, len(items))
	for i, item := range items {
		elems[i] = map[string]any{"t": serovalTypeString, "s": item}
	}
	return map[string]any{
		"t": serovalTypeArray,
		"i": 0,
		"a": elems,
		"o": 0,
	}
}

// ----------------------------------------------------------------
// 第 1 层：Seroval Streaming 拆包器
//
// 输入：  ;0x{HEX};((self.$R=…)["X"]=[],
//         ($R=> $R[33]={…}, $R[34]={…}, $R[0]=$R[33])($R["X"]))
//
// 输出：  { Definitions: {33→raw, 34→raw, 0→raw}, RootRef: 0 }
//
// 这一层完全不知道什么是 quota。它只负责从 stream 里提取
// $R[N]=VALUE 定义并确定哪个引用是最终答案（RootRef）。
// ----------------------------------------------------------------

// serovalRawDef 定义了一个 $R[N]=VALUE 的提取结果
type serovalRawDef struct {
	Index int
	Value string
}

// serovalStreamData 是 stream 拆包的输出
type serovalStreamData struct {
	Definitions map[int]string
	RootRef     int
	Cache       map[int]any // memo: resolved values keyed by ref index
}

// parseSerovalStream 解析 seroval streaming 格式。
func parseSerovalStream(text string) (*serovalStreamData, error) {
	// 定位 JS 代码段
	semi := strings.Index(text[1:], ";")
	if semi < 0 {
		return nil, fmt.Errorf("invalid seroval stream format: missing separator")
	}
	js := text[semi+2:]

	// 检查服务端错误
	if strings.Contains(js, "new Error(") {
		msg := serovalExtractError(js)
		if msg != "" {
			return nil, fmt.Errorf("server error: %s", msg)
		}
	}

	// 检查重定向（cookie 失效）
	if strings.Contains(js, "new Response(null,") {
		return nil, fmt.Errorf("cookie authentication failed: server returned redirect to login page")
	}

	// 提取所有 $R[N]=VALUE 定义
	defs := serovalExtractDefs(js)
	if len(defs) == 0 {
		return nil, fmt.Errorf("seroval stream contains no $R definitions")
	}

	data := &serovalStreamData{
		Definitions: make(map[int]string, len(defs)),
		Cache:       make(map[int]any),
	}
	for _, d := range defs {
		data.Definitions[d.Index] = d.Value
	}

	// 确定根引用：如果 $R[0] 有定义就用它，否则用编号最小者
	if _, ok := data.Definitions[0]; ok {
		data.RootRef = 0
	} else {
		min := int(^uint(0) >> 1)
		for n := range data.Definitions {
			if n < min {
				min = n
			}
		}
		data.RootRef = min
	}

	return data, nil
}

// ----------------------------------------------------------------
// $R 定义提取器
// ----------------------------------------------------------------

// serovalStripInlineDefs 从原始值字符串中删除 $R[N]= 内联定义前缀。
// 这些定义在全局已由 serovalExtractDefs 捕获，在值内部出现是冗余的。
// 删除后 Parser 不再遇到 $R[N]=，只看到纯引用 $R[N]。
func serovalStripInlineDefs(raw string) string {
	var buf strings.Builder
	buf.Grow(len(raw))

	i := 0
	inDQ := false
	inSQ := false
	esc := false

	for i < len(raw) {
		ch := raw[i]

		if esc {
			buf.WriteByte(ch)
			esc = false
			i++
			continue
		}
		if ch == '\\' {
			buf.WriteByte(ch)
			esc = true
			i++
			continue
		}
		if ch == '"' && !inSQ {
			inDQ = !inDQ
			buf.WriteByte(ch)
			i++
			continue
		}
		if ch == '\'' && !inDQ {
			inSQ = !inSQ
			buf.WriteByte(ch)
			i++
			continue
		}
		if inDQ || inSQ {
			buf.WriteByte(ch)
			i++
			continue
		}

		// 检查 $R[N]= 定义模式
		if i+2 < len(raw) && raw[i] == '$' && raw[i+1] == 'R' && raw[i+2] == '[' {
			j := i + 3
			digits := ""
			for j < len(raw) && raw[j] >= '0' && raw[j] <= '9' {
				digits += string(raw[j])
				j++
			}
			if j < len(raw) && raw[j] == ']' && j+1 < len(raw) && raw[j+1] == '=' {
				// 跳过整个 $R[N]=
				i = j + 2
				continue
			}
		}

		buf.WriteByte(ch)
		i++
	}

	return buf.String()
}

// serovalExtractDefs 扫描整个 js 中所有 $R[N]=VALUE 定义。
// 与之前版本不同：这里不跳过已消费区域——因为在同一个值块内
// 可能出现内联定义（例如 $R[0]={a:$R[1]={x:!0}}）。
// 扫描时跳过字符串内容，防止误解析字符串内部的 `$R`。
func serovalExtractDefs(js string) []serovalRawDef {
	// 第一遍：扫描所有 $R[N]= 出现的位置
	type match struct {
		index int   // N
		start int   // = 后面的位置
	}
	var locs []match
	i := 0
	for i < len(js) {
		if js[i] == '"' || js[i] == '\'' {
			end := serovalScanString(js, i, js[i])
			if end >= 0 {
				i = end + 1
				continue
			}
			i++
			continue
		}
		if i+2 < len(js) && js[i] == '$' && js[i+1] == 'R' && js[i+2] == '[' {
			j := i + 3
			digits := ""
			for j < len(js) && js[j] >= '0' && js[j] <= '9' {
				digits += string(js[j])
				j++
			}
			if j < len(js) && js[j] == ']' && j+1 < len(js) && js[j+1] == '=' {
				n, err := strconv.Atoi(digits)
				if err == nil {
					locs = append(locs, match{index: n, start: j + 2})
					i = j + 2
					continue
				}
			}
		}
		i++
	}

	// 第二遍：从每个定义位置提取值。值从 start 开始，到下一个 $R[N]= 或 js 结束为止。
	// 注意：$R[N]= 定义可以重叠（内联定义在外层定义的值内部）。
	// 我们按原始顺序扫描，自动跳过被内层定义消费过的区域。
	var defs []serovalRawDef
	processed := make(map[int]bool)

	for _, loc := range locs {
		if processed[loc.index] {
			continue
		}
		// 从 start 开始提取值
		s := js[loc.start:]
		value, _ := serovalExtractRawValue(s)
		if value != "" {
			defs = append(defs, serovalRawDef{Index: loc.index, Value: value})
			processed[loc.index] = true
		}
	}

	return defs
}

// serovalExtractRawValue 从字符串开头提取一个完整的 seroval 值。
// 返回 (值字符串, 消费字节数)。
func serovalExtractRawValue(s string) (string, int) {
	if len(s) == 0 {
		return "", 0
	}
	switch s[0] {
	case '{', '[':
		end := serovalFindCloseBracket(s, 0)
		if end < 0 {
			return "", 0
		}
		return s[:end+1], end + 1
	case '"':
		end := serovalScanString(s, 0, '"')
		if end < 0 {
			return "", 0
		}
		return s[:end+1], end + 1
	case '\'':
		end := serovalScanString(s, 0, '\'')
		if end < 0 {
			return "", 0
		}
		return s[:end+1], end + 1
	default:
		// 原语：$R[N] 引用、标识符、数字
		// 注意：] 和 } 不是分隔符——它们可以是 $R[N] 和对象/数组的一部分
		end := strings.IndexAny(s, ",")
		if end <= 0 {
			// 没有逗号，找 ) 或结尾
			end = strings.IndexByte(s, ')')
			if end <= 0 {
				end = len(s)
			}
		}
		trimmed := strings.TrimSpace(s[:end])
		return trimmed, len(trimmed)
	}
}

// ----------------------------------------------------------------
// 第 2 层：词法分析器 + 语法解析器
//
// 输入：seroval JS 字面量字符串（一个完整的值）
// 输出：Go 原生值（map[string]any, []any, string, float64, bool, nil, serovalRef）
//
// 这不是字符串替换——这是真正的递归下降解析器，逐字符扫描生成 AST。
// 在解析阶段会保留 $R[N] 引用为 serovalRef 节点，等 RefResolver 展平。
// ----------------------------------------------------------------

// serovalTokenType 定义词法单元类型
type serovalTokenType int

const (
	tokEOF serovalTokenType = iota
	tokLBRACE               // {
	tokRBRACE               // }
	tokLBRACKET             // [
	tokRBRACKET             // ]
	tokCOLON                // :
	tokCOMMA                // ,
	tokIDENT                // 裸标识符
	tokSTRING               // "..." 或 '...'
	tokNUMBER               // 数字
	tokTRUE                 // true
	tokFALSE                // false
	tokNULL                 // null
	tokREFERENCE            // $R[N]
	tokBANG                 // !0 或 !1（上下文决定）
)

// serovalToken 表示一个词法单元
type serovalToken struct {
	typ  serovalTokenType
	text string  // 原始文本（用于 IDENT、STRING）
	num  float64 // 用于 NUMBER
	ref  int     // 用于 REFERENCE
}

// serovalLexer 是 seroval JS 字面量的词法分析器
type serovalLexer struct {
	input string
	pos   int
	start int
}

func newSerovalLexer(input string) *serovalLexer {
	return &serovalLexer{input: input}
}

func (l *serovalLexer) peek() byte {
	if l.pos >= len(l.input) {
		return 0
	}
	return l.input[l.pos]
}

func (l *serovalLexer) next() byte {
	if l.pos >= len(l.input) {
		return 0
	}
	ch := l.input[l.pos]
	l.pos++
	return ch
}

func (l *serovalLexer) skipWS() {
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			l.pos++
		} else {
			break
		}
	}
}

func (l *serovalLexer) NextToken() serovalToken {
	l.skipWS()
	if l.pos >= len(l.input) {
		return serovalToken{typ: tokEOF}
	}

	ch := l.next()

	switch ch {
	case '{':
		return serovalToken{typ: tokLBRACE}
	case '}':
		return serovalToken{typ: tokRBRACE}
	case '[':
		return serovalToken{typ: tokLBRACKET}
	case ']':
		return serovalToken{typ: tokRBRACKET}
	case ':':
		return serovalToken{typ: tokCOLON}
	case ',':
		return serovalToken{typ: tokCOMMA}
	case '!':
		// !0 → false, !1 → true
		if l.pos < len(l.input) {
			if l.input[l.pos] == '0' {
				l.pos++
				return serovalToken{typ: tokFALSE}
			}
			if l.input[l.pos] == '1' {
				l.pos++
				return serovalToken{typ: tokTRUE}
			}
		}
		// 不是 !0/!1，返回单个 !
		return serovalToken{typ: tokIDENT, text: "!"}
	case '"', '\'':
		// 字符串：同时定位结束位置和解转义
		quote := ch
		var inner strings.Builder
		for l.pos < len(l.input) {
			c := l.input[l.pos]
			if c == '\\' {
				l.pos++
				if l.pos < len(l.input) {
					next := l.input[l.pos]
					switch next {
					case '"':
						inner.WriteByte('"')
					case '\'':
						inner.WriteByte('\'')
					case '\\':
						inner.WriteByte('\\')
					case 'n':
						inner.WriteByte('\n')
					case 't':
						inner.WriteByte('	')
					case 'r':
						inner.WriteByte('\r')
					default:
						inner.WriteByte('\\')
						inner.WriteByte(next)
					}
					l.pos++
				}
			} else if c == quote {
				l.pos++
				return serovalToken{typ: tokSTRING, text: inner.String()}
			} else {
				inner.WriteByte(c)
				l.pos++
			}
		}
		// 字符串未闭合
		return serovalToken{typ: tokIDENT, text: string(ch)}
	case '$':
		// 可能是 $R[N]
		if l.peek() == 'R' {
			l.pos++ // 跳过 R
			if l.peek() == '[' {
				l.pos++ // 跳过 [
				digits := ""
				for l.pos < len(l.input) && l.input[l.pos] >= '0' && l.input[l.pos] <= '9' {
					digits += string(l.input[l.pos])
					l.pos++
				}
				if l.pos < len(l.input) && l.input[l.pos] == ']' && len(digits) > 0 {
					n, err := strconv.Atoi(digits)
					if err == nil {
						l.pos++ // 跳过 ]
						return serovalToken{typ: tokREFERENCE, ref: n}
					}
				}
				// 解析失败，回溯——把 $ 当标识符
				// 但我们已经消费了 $R[digits，退不回去了
				// 返回 IDENT
				return serovalToken{typ: tokIDENT, text: "$R[" + digits}
			}
			// $R 后面不是 [——可能是标识符的一部分
		}
		// 标识符：从 $ 开始扫描
		start := l.pos - 1
		for l.pos < len(l.input) && serovalIsIdentCont(l.input[l.pos]) {
			l.pos++
		}
		return serovalToken{typ: tokIDENT, text: l.input[start:l.pos]}
	default:
		// 数字或标识符
		if ch >= '0' && ch <= '9' || ch == '-' || ch == '+' {
			start := l.pos - 1
			hasDot := false
			hasExp := false
			for l.pos < len(l.input) {
				c := l.input[l.pos]
				if c >= '0' && c <= '9' {
					l.pos++
				} else if c == '.' && !hasDot {
					hasDot = true
					l.pos++
				} else if (c == 'e' || c == 'E') && !hasExp {
					hasExp = true
					l.pos++
					if l.pos < len(l.input) && (l.input[l.pos] == '-' || l.input[l.pos] == '+') {
						l.pos++
					}
				} else {
					break
				}
			}
			numStr := l.input[start:l.pos]
			if f, err := strconv.ParseFloat(numStr, 64); err == nil {
				return serovalToken{typ: tokNUMBER, num: f}
			}
			// 不是有效数字，作为标识符
			return serovalToken{typ: tokIDENT, text: numStr}
		}
		// 标识符
		start := l.pos - 1
		for l.pos < len(l.input) && serovalIsIdentCont(l.input[l.pos]) {
			l.pos++
		}
		ident := l.input[start:l.pos]
		switch strings.ToLower(ident) {
		case "true":
			return serovalToken{typ: tokTRUE}
		case "false":
			return serovalToken{typ: tokFALSE}
		case "null":
			return serovalToken{typ: tokNULL}
		default:
			return serovalToken{typ: tokIDENT, text: ident}
		}
	}
}

// ----------------------------------------------------------------
// 递归下降解析器
// ----------------------------------------------------------------

// serovalASTNode 可以是：
//
//	map[string]any   — JS 对象
//	[]any            — JS 数组
//	string           — 字符串
//	float64          — 数字
//	bool             — 布尔
//	nil              — null
//	serovalRef       — 未解析的 $R[N] 引用（后续由 Resolver 展开）
type serovalRef struct {
	Index int
}

// serovalParseValue 解析一个完整的 seroval JS 值。
func serovalParseValue(lex *serovalLexer) (any, error) {
	tok := lex.NextToken()
	return serovalParseValueWithToken(lex, tok)
}

// serovalParseValueWithToken 使用已经读取的 token 继续解析
func serovalParseValueWithToken(lex *serovalLexer, tok serovalToken) (any, error) {
	switch tok.typ {
	case tokLBRACE:
		return serovalParseObject(lex)
	case tokLBRACKET:
		return serovalParseArray(lex)
	case tokSTRING:
		return tok.text, nil
	case tokNUMBER:
		return tok.num, nil
	case tokTRUE:
		return true, nil
	case tokFALSE:
		return false, nil
	case tokNULL:
		return nil, nil
	case tokREFERENCE:
		// Pure reference — Stream 层已剥离内联 $R[N]= 定义
		return serovalRef{Index: tok.ref}, nil
	case tokIDENT:
		// 裸标识符作为字符串值（seroval 语法允许）
		return tok.text, nil
	case tokEOF:
		return nil, fmt.Errorf("unexpected end of input")
	default:
		return nil, fmt.Errorf("unexpected token type %v with text %q", tok.typ, tok.text)
	}
}

// serovalParseObject 解析 { key:value, ... }
func serovalParseObject(lex *serovalLexer) (map[string]any, error) {
	obj := make(map[string]any)

	for {
		tok := lex.NextToken()
		if tok.typ == tokRBRACE {
			break
		}
		if tok.typ == tokCOMMA {
			continue
		}
		if tok.typ == tokEOF {
			return nil, fmt.Errorf("unexpected EOF in object")
		}

		// Key
		var key string
		switch tok.typ {
		case tokSTRING:
			key = tok.text
		case tokIDENT:
			key = tok.text
		case tokNUMBER:
			key = fmt.Sprintf("%g", tok.num)
		default:
			return nil, fmt.Errorf("unexpected token %v as object key", tok.typ)
		}

		// 冒号
		colon := lex.NextToken()
		if colon.typ != tokCOLON {
			return nil, fmt.Errorf("expected ':' after object key %q, got token type %v", key, colon.typ)
		}

		// Value
		value, err := serovalParseValue(lex)
		if err != nil {
			return nil, fmt.Errorf("parse value for key %q: %w", key, err)
		}
		obj[key] = value
	}

	return obj, nil
}

// serovalParseArray 解析 [ item, ... ]
func serovalParseArray(lex *serovalLexer) ([]any, error) {
	var arr []any

	for {
		tok := lex.NextToken()
		if tok.typ == tokRBRACKET {
			break
		}
		if tok.typ == tokCOMMA {
			continue
		}
		if tok.typ == tokEOF {
			return nil, fmt.Errorf("unexpected EOF in array")
		}

		value, err := serovalParseValueWithToken(lex, tok)
		if err != nil {
			return nil, fmt.Errorf("parse array item: %w", err)
		}
		arr = append(arr, value)
	}

	return arr, nil
}

// ----------------------------------------------------------------
// 第 3 层：引用解析器
//
// 输入：RootRef + StreamData（原始定义字符串）
// 输出：完全解析的 Go 原生值（所有 $R[N] 已展开）
//
// 支持：
//   - 直接引用：$R[0]=$R[33] → 跟到 $R[33]={…}
//   - 别名链：$R[1]=$R[2], $R[2]=$R[3] → $R[1] → $R[3]={…}
//   - 内联引用：{a:$R[1], b:$R[1]} → 同一对象展开两次
//   - 循环检测：$R[1]=$R[2], $R[2]=$R[1] → 报错
// ----------------------------------------------------------------

// serovalResolve 从 RootRef 开始解析整个引用链。
// resolving 用于循环检测，外部调用传 nil。
func serovalResolve(ref int, data *serovalStreamData, resolving map[int]bool) (any, error) {
	if resolving == nil {
		resolving = make(map[int]bool)
	}

	if resolving[ref] {
		return nil, fmt.Errorf("circular reference detected at $R[%d]", ref)
	}

	// 检查 memo cache
	if cached, ok := data.Cache[ref]; ok {
		return cached, nil
	}

	raw, ok := data.Definitions[ref]
	if !ok {
		return nil, fmt.Errorf("$R[%d] is not defined", ref)
	}

	// 如果值是冒泡引用（$R[N]=$R[M]）
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "$R[") && strings.HasSuffix(trimmed, "]") {
		inner := trimmed[3 : len(trimmed)-1]
		target, err := strconv.Atoi(inner)
		if err == nil {
			resolving[ref] = true
			return serovalResolve(target, data, resolving)
		}
	}

	resolving[ref] = true

	// 剥离内联 $R[N]= 定义（Stream 层语法，不传给 Parser）
	raw = serovalStripInlineDefs(raw)

	lex := newSerovalLexer(raw)
	ast, err := serovalParseValue(lex)
	if err != nil {
		delete(resolving, ref)
		return nil, fmt.Errorf("parse $R[%d]: %w", ref, err)
	}

	resolved, err := serovalResolveAST(ast, data, resolving)
	if err != nil {
		delete(resolving, ref)
		return nil, err
	}

	// memo cache
	data.Cache[ref] = resolved
	return resolved, nil
}

// serovalResolveAST 递归遍历 AST，将所有 serovalRef 替换为解析值。
func serovalResolveAST(node any, data *serovalStreamData, resolving map[int]bool) (any, error) {
	switch v := node.(type) {
	case serovalRef:
		// 引用展开
		return serovalResolve(v.Index, data, resolving)

	case map[string]any:
		res := make(map[string]any, len(v))
		for key, val := range v {
			resolved, err := serovalResolveAST(val, data, resolving)
			if err != nil {
				return nil, fmt.Errorf("resolve key %q: %w", key, err)
			}
			res[key] = resolved
		}
		return res, nil

	case []any:
		res := make([]any, len(v))
		for i, val := range v {
			resolved, err := serovalResolveAST(val, data, resolving)
			if err != nil {
				return nil, fmt.Errorf("resolve index %d: %w", i, err)
			}
			res[i] = resolved
		}
		return res, nil

	default:
		return v, nil
	}
}

// ----------------------------------------------------------------
// 服务端错误提取
// ----------------------------------------------------------------

func serovalExtractError(js string) string {
	needle := `new Error("`
	idx := strings.Index(js, needle)
	if idx < 0 {
		return ""
	}
	start := idx + len(needle)
	end := serovalScanString(js, start-1, '"')
	if end < 0 || end <= start {
		return ""
	}
	raw := js[start:end]
	raw = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t").Replace(raw)
	return strings.TrimSpace(raw)
}

// ----------------------------------------------------------------
// 通用辅助函数
// ----------------------------------------------------------------

// serovalScanString 寻找匹配的引号结束位置（处理转义）。
func serovalScanString(s string, start int, quote byte) int {
	for i := start + 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			return i
		}
	}
	return -1
}

// serovalFindCloseBracket 找到匹配的 } 或 ]（正确追踪字符串和嵌套括号）。
func serovalFindCloseBracket(s string, start int) int {
	if start >= len(s) {
		return -1
	}
	open := s[start]
	var close byte
	switch open {
	case '{':
		close = '}'
	case '[':
		close = ']'
	default:
		return -1
	}
	depth := 1
	inDQ := false
	inSQ := false
	esc := false
	for i := start + 1; i < len(s); i++ {
		ch := s[i]
		if esc {
			esc = false
			continue
		}
		if ch == '\\' {
			esc = true
			continue
		}
		if ch == '"' && !inSQ {
			inDQ = !inDQ
			continue
		}
		if ch == '\'' && !inDQ {
			inSQ = !inSQ
			continue
		}
		if inDQ || inSQ {
			continue
		}
		if ch == open {
			depth++
		} else if ch == close {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func serovalIsIdentCont(ch byte) bool {
	return unicode.IsLetter(rune(ch)) || unicode.IsDigit(rune(ch)) || ch == '_' || ch == '$'
}

// ----------------------------------------------------------------
// 第 4 层：顶层分发函数
//
// 这一层判断响应格式、决定如何解析、将 Go 原生值转换到业务类型。
// ----------------------------------------------------------------

// parseSerovalResponse 判断响应格式并分发。
//
// 三种可能：
//
//	① seroval streaming:  ;0x{HEX};((…)($R[…]))
//	② plain JSON:		   {…} 或 […]
//	③ HTML:			   <!DOCTYPE html>…
func parseSerovalResponse(raw []byte) (json.RawMessage, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, fmt.Errorf("empty response")
	}

	if looksLikeHTML(raw) {
		return nil, fmt.Errorf("response contains HTML login page (cookie may be invalid or expired)")
	}

	// Plain JSON
	if text[0] == '{' || text[0] == '[' {
		return raw, nil
	}

	// Seroval streaming
	if strings.HasPrefix(text, serovalStreamPrefix) {
		stream, err := parseSerovalStream(text)
		if err != nil {
			return nil, err
		}
		root, err := serovalResolve(stream.RootRef, stream, nil)
		if err != nil {
			return nil, fmt.Errorf("seroval resolve: %w", err)
		}
		return json.Marshal(root)
	}

	return nil, fmt.Errorf("unrecognized response format: %s", serovalShorten(text, 100))
}

// parseSerovalWorkspaces 解析工作区列表。
func parseSerovalWorkspaces(raw []byte) ([]OpenCodeWorkspace, error) {
	jsonBytes, err := parseSerovalResponse(raw)
	if err != nil {
		return nil, err
	}

	var items []map[string]any
	if err := json.Unmarshal(jsonBytes, &items); err != nil {
		return nil, fmt.Errorf("decode workspaces: %w", err)
	}

	seen := map[string]OpenCodeWorkspace{}
	for _, item := range items {
		id, _ := item["id"].(string)
		name, _ := item["name"].(string)
		if id != "" {
			addWorkspaceCandidate(seen, id, name)
		}
	}
	return workspaceCandidateList(seen), nil
}

// parseSerovalQuota 解析 Go 限额。
func parseSerovalQuota(raw []byte) (*GoQuotaResponse, error) {
	// 先尝试旧版 SSD 回退（工作区页面内联数据 / 任意文本中的匹配）
	// 这支持 $R[N](...) 函数调用格式和正则匹配
	if result := tryParseQuotaFallback(string(raw)); result != nil {
		return result, nil
	}

	jsonBytes, err := parseSerovalResponse(raw)
	if err != nil {
		return nil, err
	}

	var result GoQuotaResponse
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return nil, fmt.Errorf("decode quota: %w\njson: %s", err, serovalShorten(string(jsonBytes), 300))
	}
	result.Raw = jsonBytes

	if result.RollingUsage == nil || result.WeeklyUsage == nil || result.MonthlyUsage == nil {
		return nil, fmt.Errorf("quota response missing usage buckets: %s", serovalShorten(string(jsonBytes), 200))
	}
	return &result, nil
}

// ----------------------------------------------------------------
// 旧版 SSR 回退 — 工作区页面内联数据
// ----------------------------------------------------------------

func tryParseQuotaFallback(text string) *GoQuotaResponse {
	match := hydratedQuotaPattern.FindStringSubmatch(text)
	if len(match) != 12 {
		return nil
	}

	mkBucket := func(status, reset, percent string) (*GoQuotaBucket, error) {
		ri, err := strconv.ParseInt(reset, 10, 64)
		if err != nil {
			return nil, err
		}
		up, err := strconv.Atoi(percent)
		if err != nil {
			return nil, err
		}
		return &GoQuotaBucket{Status: status, ResetInSec: ri, UsagePercent: up}, nil
	}

	rolling, err := mkBucket(match[3], match[4], match[5])
	if err != nil {
		return nil
	}
	weekly, err := mkBucket(match[6], match[7], match[8])
	if err != nil {
		return nil
	}
	monthly, err := mkBucket(match[9], match[10], match[11])
	if err != nil {
		return nil
	}

	return &GoQuotaResponse{
		Mine:         match[1] == "0",
		UseBalance:   match[2] == "0",
		RollingUsage: rolling,
		WeeklyUsage:  weekly,
		MonthlyUsage: monthly,
		Raw:          json.RawMessage("{}"),
	}
}

func serovalShorten(s string, max int) string {
	cleaned := strings.Join(strings.Fields(s), " ")
	if max <= 0 || len(cleaned) <= max {
		return cleaned
	}
	return cleaned[:max] + "..."
}
