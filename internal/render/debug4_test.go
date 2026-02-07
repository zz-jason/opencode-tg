package render

import (
	"fmt"
	"testing"
)

func TestDebugCodeEscape(t *testing.T) {
	input := "`a < b && c > d`"

	// 测试 renderInline 直接
	result := renderInline(input)
	fmt.Printf("renderInline(%q) = %q\n", input, result)

	// 检查是否包含 <code>
	if !containsCodeTag(result) {
		t.Errorf("Result should contain <code> tag")
	}
}

func containsCodeTag(s string) bool {
	// 检查是否包含未转义的 <code>
	return contains(s, "<code>") && contains(s, "</code>")
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
