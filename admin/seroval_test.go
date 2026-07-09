package admin

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSeroval_BasicResponse(t *testing.T) {
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]={mine:!0,useBalance:!0,rollingUsage:{status:"ok",resetInSec:3600,usagePercent:0},weeklyUsage:{status:"ok",resetInSec:604800,usagePercent:0},monthlyUsage:{status:"ok",resetInSec:2592000,usagePercent:0}})($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	if data["mine"] != false {
		t.Fatalf("mine should be false, got %#v", data["mine"])
	}
	t.Logf("OK basic: %s", string(result))
}

func TestSeroval_SharedRef(t *testing.T) {
	// 两个字段引用同一个 $R[1]
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]={a:$R[1]={x:!0},b:$R[1]})($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	// a 和 b 应该都是 {"x":false}
	t.Logf("OK shared ref: %s", string(result))
}

func TestSeroval_AliasChain(t *testing.T) {
	// 别名链：$R[0]=$R[1]=$R[2]={...}
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[3]={v:1},$R[2]=$R[3],$R[1]=$R[2],$R[0]=$R[1])($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	if data["v"] != 1.0 {
		t.Fatalf("v should be 1.0, got %#v", data["v"])
	}
	t.Logf("OK chain: %s", string(result))
}

func TestSeroval_CircularRefReturnsError(t *testing.T) {
	// 循环引用：$R[1]=$R[2], $R[2]=$R[1]
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[1]=$R[2],$R[2]=$R[1],$R[0]=$R[1])($R["server-fn:0"]))`
	_, err := parseSerovalResponse([]byte(js))
	if err == nil {
		t.Fatal("expected circular reference error, got nil")
	}
	if !contains(err.Error(), "circular") {
		t.Fatalf("error should mention circular, got: %v", err)
	}
	t.Logf("OK circular: %v", err)
}

func TestSeroval_StringContainsRefLiteral(t *testing.T) {
	// 字符串内部的 $R[0]= 不应被解析
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]={key:"$R[0]=literal",text:"test $R[1]= value"})($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	if data["key"] != "$R[0]=literal" {
		t.Fatalf("key should be literal, got %#v", data["key"])
	}
	t.Logf("OK literal: %s", string(result))
}

func TestSeroval_PlainJSON(t *testing.T) {
	raw := []byte(`{"mine":true,"useBalance":false,"rollingUsage":{"status":"ok","resetInSec":3600,"usagePercent":0}}`)
	result, err := parseSerovalResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	t.Logf("OK plain JSON: %s", string(result))
}

func TestSeroval_ErrorResponse(t *testing.T) {
	raw := ";0x00000260;((self.$R=self.$R||{})[\"server-fn:0\"]=[],($R=>$R[0]=Object.assign(new Error(\"actor of type \\\"public\\\" is not associated with an account\"),{stack:\"Error\"}))($R[\"server-fn:0\"]))"
	_, err := parseSerovalResponse([]byte(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "actor of type") {
		t.Fatalf("error should contain message, got: %v", err)
	}
	t.Logf("OK error: %v", err)
}

func TestSeroval_ArrayResponse(t *testing.T) {
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]=[{id:"wrk_1",name:"First"},{id:"wrk_2",name:"Second"}])($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var arr []any
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 items, got %d", len(arr))
	}
	t.Logf("OK array: %s", string(result))
}

func TestSeroval_WorkspacePageInline(t *testing.T) {
	// 来自工作区页面的真实 SSR 内联数据格式
	raw := []byte(`$R[28]($R[18],$R[33]={mine:!0,useBalance:!1,rollingUsage:$R[34]={status:"ok",resetInSec:18000,usagePercent:0},weeklyUsage:$R[35]={status:"ok",resetInSec:490728,usagePercent:15},monthlyUsage:$R[36]={status:"ok",resetInSec:2591627,usagePercent:7}});`)
	// 该格式不是完整的 seroval stream（无 ;0x 前缀），走旧版正则 fallback
	result, err := parseSerovalQuota(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.WeeklyUsage == nil || result.WeeklyUsage.UsagePercent != 15 {
		t.Fatalf("unexpected weekly: %#v", result.WeeklyUsage)
	}
	t.Logf("OK page: %+v", result)
}

func TestSeroval_NestedQuote(t *testing.T) {
	// 嵌套引号和转义
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]={msg:"hello \"world\""})($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	if data["msg"] != `hello "world"` {
		t.Fatalf("expected message with quotes, got %#v", data["msg"])
	}
	t.Logf("OK nested quote: %s", string(result))
}

func TestSeroval_NumericKeys(t *testing.T) {
	// 数字 key（seroval 允许：{0:"a",1:"b"}）
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]={0:"a",1:"b"})($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	if data["0"] != "a" || data["1"] != "b" {
		t.Fatalf("unexpected numeric keys: %#v", data)
	}
	t.Logf("OK numeric keys: %s", string(result))
}

// TestSeroval_EscapedRefInString — 字符串内容中的 $R[N]= 不应被识别为引用
// 例如 {msg:"$R[33]=\"abc\""}，这里的 $R[33]=" 是普通文本
func TestSeroval_EscapedRefInString(t *testing.T) {
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]={msg:"$R[33]=\"abc\\\"def\""})($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	expected := `$R[33]="abc\"def"`
	if data["msg"] != expected {
		t.Fatalf("expected %q, got %q", expected, data["msg"])
	}
	t.Logf("OK escaped ref in string: %s", string(result))
}

// TestSeroval_ReservedWordsAsKeys — JS 保留字作为对象 key
// seroval 允许裸 {default:1, class:2, function:3}
func TestSeroval_ReservedWordsAsKeys(t *testing.T) {
	js := `;0xFFFF;((self.$R=self.$R||{})["server-fn:0"]=[],($R=>$R[0]={default:1,class:2,function:3})($R["server-fn:0"]))`
	result, err := parseSerovalResponse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("unmarshal: %v\njson: %s", err, string(result))
	}
	if data["default"] != 1.0 {
		t.Fatalf("default should be 1, got %#v", data["default"])
	}
	if data["class"] != 2.0 {
		t.Fatalf("class should be 2, got %#v", data["class"])
	}
	if data["function"] != 3.0 {
		t.Fatalf("function should be 3, got %#v", data["function"])
	}
	t.Logf("OK reserved words as keys: %s", string(result))
}

// TestSeroval_GoldenFiles 用 testdata/ 中的真实请求/响应样本驱动解析测试。
// 每个 .txt 文件是一个独立的 seroval 响应串。测试约定：
//   - 如果存在同名 .json 文件：解析必须成功且输出 JSON 完全匹配。
//   - 如果存在同名 .error 文件：解析必须失败且错误消息包含 .error 文件内容。
//   - 如果两者都不存在：解析必须成功，仅检查可反序列化。
func TestSeroval_GoldenFiles(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".txt")
		t.Run(base, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + e.Name())
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			raw = bytes.TrimSpace(raw)

			result, parseErr := parseSerovalResponse(raw)

			// 检查 error golden
			errorPath := "testdata/" + base + ".error"
			if _, err := os.Stat(errorPath); err == nil {
				// 期望解析失败
				if parseErr == nil {
					t.Fatalf("expected error, got success: %s", string(result))
				}
				expectedError, _ := os.ReadFile(errorPath)
				if !bytes.Contains([]byte(parseErr.Error()), bytes.TrimSpace(expectedError)) {
					t.Fatalf("error mismatch:\ngot:  %v\nexpect contains: %s", parseErr, string(expectedError))
				}
				t.Logf("OK golden error: %s → %v", e.Name(), parseErr)
				return
			}

			// 期望成功
			if parseErr != nil {
				t.Fatalf("parse %s: %v", e.Name(), parseErr)
			}

			// 检查是否可反序列化
			var v any
			if err := json.Unmarshal(result, &v); err != nil {
				t.Fatalf("unmarshal %s: %v\njson: %s", e.Name(), err, string(result))
			}

			// 检查 JSON golden
			goldenPath := "testdata/" + base + ".json"
			if _, err := os.Stat(goldenPath); err == nil {
				golden, err := os.ReadFile(goldenPath)
				if err != nil {
					t.Fatalf("read golden: %v", err)
				}
				golden = bytes.TrimSpace(golden)
				if !bytes.Equal(bytes.TrimSpace(result), golden) {
					t.Fatalf("mismatch:\ngot:  %s\nexpect:%s", string(result), string(golden))
				}
				t.Logf("OK golden match: %s", e.Name())
			} else {
				t.Logf("OK no golden, parsed: %s", string(result))
			}
		})
	}
}

// 辅助函数
func contains(s, substr string) bool {
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
