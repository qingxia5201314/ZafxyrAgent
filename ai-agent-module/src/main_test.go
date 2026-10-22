package main

import (
	"strings"
	"testing"
)

func TestParseBounds(t *testing.T) {
	cases := []struct {
		in             string
		x1, y1, x2, y2 int
		ok             bool
	}{
		{"[0,0][1220,2712]", 0, 0, 1220, 2712, true},
		{"[60,180][240,260]", 60, 180, 240, 260, true},
		{"[80,300][1000,400]", 80, 300, 1000, 400, true},
		{"", 0, 0, 0, 0, false},
		{"garbage", 0, 0, 0, 0, false},
		{"[1,2][3]", 0, 0, 0, 0, false},
	}
	for _, c := range cases {
		x1, y1, x2, y2, ok := parseBounds(c.in)
		if ok != c.ok || x1 != c.x1 || y1 != c.y1 || x2 != c.x2 || y2 != c.y2 {
			t.Errorf("parseBounds(%q) = %d,%d,%d,%d,%v; 期望 %d,%d,%d,%d,%v",
				c.in, x1, y1, x2, y2, ok, c.x1, c.y1, c.x2, c.y2, c.ok)
		}
	}
}

// 结构照着真机上 dump 出来的 XML 写（Redmi 2407FRK8EC / Android 16）。
const sampleXML = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0"><node index="0" text="" resource-id="" class="android.widget.FrameLayout" package="com.tencent.mm" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,0][1220,2712]" drawing-order="0" hint=""><node index="1" text="微信" resource-id="com.tencent.mm:id/title" class="android.widget.TextView" package="com.tencent.mm" content-desc="" checkable="false" checked="false" clickable="false" enabled="true" focusable="false" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[60,180][240,260]" drawing-order="1" hint="" /><node index="2" text="" resource-id="com.tencent.mm:id/search" class="android.widget.EditText" package="com.tencent.mm" content-desc="搜索" checkable="false" checked="false" clickable="true" enabled="true" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[80,300][1000,400]" drawing-order="2" hint="搜索" /><node index="3" text="聊天记录" resource-id="" class="android.widget.Button" package="com.tencent.mm" content-desc="" checkable="false" checked="false" clickable="true" enabled="false" focusable="true" focused="false" scrollable="false" long-clickable="false" password="false" selected="false" bounds="[0,500][1220,600]" drawing-order="3" hint="" /></node></hierarchy>`

func TestParseUILines(t *testing.T) {
	lines, err := parseUILines([]byte(sampleXML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("期望 3 行，得到 %d 行: %v", len(lines), lines)
	}
	// 第 1 个：text 属性，坐标取中心 (60+240)/2=150, (180+260)/2=220
	if !strings.Contains(lines[0], "微信") || !strings.Contains(lines[0], "@(150,220)") {
		t.Errorf("第 1 行不对: %q", lines[0])
	}
	// 第 2 个：text 为空，应回退到 content-desc；且标可点
	if !strings.Contains(lines[1], "搜索") || !strings.Contains(lines[1], "可点") {
		t.Errorf("第 2 行不对: %q", lines[1])
	}
	if !strings.Contains(lines[1], "@(540,350)") {
		t.Errorf("第 2 行坐标不对: %q", lines[1])
	}
	// 第 3 个：enabled=false 应标禁用
	if !strings.Contains(lines[2], "禁用") {
		t.Errorf("第 3 行应标禁用: %q", lines[2])
	}
}

func TestParseUILinesEmpty(t *testing.T) {
	lines, err := parseUILines([]byte(`<hierarchy></hierarchy>`))
	if err != nil {
		t.Fatalf("空树不该报错: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("期望 0 行，得到 %v", lines)
	}
}

func TestParseUILinesMalformed(t *testing.T) {
	if _, err := parseUILines([]byte(`<hierarchy><node text="x"`)); err == nil {
		t.Error("残缺 XML 应该报错")
	}
}

// keyTool 把参数拼进 shell，必须挡住注入。这些应该在执行命令前就被拒绝。
func TestKeyToolRejectsInjection(t *testing.T) {
	bad := []string{
		"3; rm -rf /",
		"HOME && reboot",
		"$(id)",
		"`id`",
		"3|ls",
		"abc",
		"",
		"HOME;touch /tmp/pwned",
	}
	for _, b := range bad {
		if got, err := keyTool(b); err == nil {
			t.Errorf("keyTool(%q) 应该被拒绝，却返回 %q", b, got)
		}
	}
}

func TestTapToolRejectsBadCoords(t *testing.T) {
	if _, err := tapTool(-1, 10, 0); err == nil {
		t.Error("负坐标应该被拒绝")
	}
	if _, err := tapTool(10, 999999, 0); err == nil {
		t.Error("超大坐标应该被拒绝")
	}
}

func TestKeyNamesCoverage(t *testing.T) {
	for _, k := range []string{"HOME", "BACK", "ENTER", "POWER", "TAB", "DEL", "MENU", "RECENT"} {
		if _, ok := keyNames[k]; !ok {
			t.Errorf("keyNames 缺少常用键 %s", k)
		}
	}
}
