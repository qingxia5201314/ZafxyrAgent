package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type Config struct {
	Provider   string   `json:"provider"`
	APIKey     string   `json:"api_key"`
	BaseURL    string   `json:"base_url"`
	Model      string   `json:"model"`
	System     string   `json:"system"`
	Port       int      `json:"port"`
	AutoOpen   bool     `json:"autoopen"`
	AllowShell *bool    `json:"allow_shell"`
	MaxTokens  int      `json:"max_tokens"`
	Fallback   []string `json:"fallback_models"`
	Models     []string `json:"models"`
}

type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var (
	cfg    Config
	mu     sync.Mutex
	hist   []Msg
	modDir string
	home   string

	evMu   sync.Mutex
	evSubs = map[chan []byte]struct{}{}

	actMu       sync.Mutex
	lastActions []string
)

type event struct {
	Type        string   `json:"type"`
	Text        string   `json:"text,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
	ID          int      `json:"id,omitempty"`
	Name        string   `json:"name,omitempty"`
	Label       string   `json:"label,omitempty"`
	Result      string   `json:"result,omitempty"`
	State       string   `json:"state,omitempty"`
}

var toolSeq int64

func nextToolID() int {
	return int(atomic.AddInt64(&toolSeq, 1))
}

// truncate 按「字符」而不是字节截断。
// 中文一个字占 3 个字节，按字节切会把 UTF-8 序列劈成两半，输出乱码
// （真机上 screen 返回的节点文字就踩过这个：会显示成 "..." · �...）。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func broadcast(e event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	evMu.Lock()
	for ch := range evSubs {
		select {
		case ch <- b:
		default:
		}
	}
	evMu.Unlock()
}

func status(format string, a ...interface{}) {
	txt := fmt.Sprintf(format, a...)
	if txt == "" {
		return
	}
	log.Printf("[status] %s", txt)
	broadcast(event{Type: "status", Text: txt})
}

func noteAction(name string) {
	actMu.Lock()
	lastActions = append(lastActions, name)
	actMu.Unlock()
}

func resetActions() {
	actMu.Lock()
	lastActions = nil
	actMu.Unlock()
}

func snapshotActions() []string {
	actMu.Lock()
	defer actMu.Unlock()
	out := make([]string, len(lastActions))
	copy(out, lastActions)
	return out
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan []byte, 32)
	evMu.Lock()
	evSubs[ch] = struct{}{}
	evMu.Unlock()
	defer func() {
		evMu.Lock()
		delete(evSubs, ch)
		evMu.Unlock()
	}()

	fmt.Fprintf(w, "event: ready\ndata: {}\n\n")
	fl.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

// Android dengan Private DNS (DoT) tidak menyediakan resolver loopback
// (127.0.0.1:53/[::1]:53) dan /etc/resolv.conf tidak ada, sehingga resolver
// murni Go gagal. Dial kustom meminta DNS publik secara langsung.
func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			servers := []string{
				"8.8.8.8:53",
				"8.8.4.4:53",
				"1.1.1.1:53",
				"1.0.0.1:53",
				"94.140.14.14:53",
				"192.168.100.1:53",
			}
			var lastErr error
			for _, s := range servers {
				d := net.Dialer{Timeout: 3 * time.Second}
				c, err := d.DialContext(ctx, "udp", s)
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
}

func httpClient(timeout time.Duration) *http.Client {
	pool, _ := x509.SystemCertPool()
	if pool == nil {
		pool = x509.NewCertPool()
	}
	if pem, err := os.ReadFile(filepath.Join(modDir, "cacert.pem")); err == nil {
		pool.AppendCertsFromPEM(pem)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// configPath 是用户可编辑的配置位置。
// 放在 home（/data/adb/aiagent/）而不是模块目录，模块升级时不会被解压覆盖。
func configPath() string {
	return filepath.Join(home, "config.json")
}

func loadConfig() {
	// 优先读用户目录；读不到再回退模块目录里的出厂模板，
	// 兼容老版本把配置写在模块目录里的情况。
	candidates := []string{configPath(), filepath.Join(modDir, "config.json")}
	var (
		b    []byte
		err  error
		used string
	)
	for _, p := range candidates {
		if b, err = os.ReadFile(p); err == nil {
			used = p
			break
		}
	}
	// 默认自动打开聊天界面。放在读取之前设置，这样配置文件里没有 autoopen
	// 字段时也能保持 true（原来只在读取成功后设置，配置缺失就变 false）。
	cfg.AutoOpen = true
	if used == "" {
		// 原来这里直接 return，导致配置缺失时所有默认值都不生效
		// （最明显的后果：Port 保持 0，服务绑到随机端口，用户完全连不上）。
		// 改成记一条日志后继续走默认值。
		log.Printf("config read error: %v（继续使用默认配置）", err)
	} else {
		log.Printf("config loaded: %s", used)
		if e := json.Unmarshal(b, &cfg); e != nil {
			log.Printf("config parse error: %v", e)
		}
	}
	prov := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch prov {
	case "", "gemini", "google":
		prov = "gemini"
	case "openrouter", "openai-compatible", "openai_compatible", "openai", "oaicompat":
		prov = "openai"
	}
	cfg.Provider = prov
	if cfg.BaseURL == "" {
		if cfg.Provider == "openai" {
			cfg.BaseURL = "https://openrouter.ai/api/v1"
		} else {
			cfg.BaseURL = "https://generativelanguage.googleapis.com/v1beta"
		}
	}
	if cfg.Model == "" {
		if cfg.Provider == "openai" {
			cfg.Model = "google/gemini-2.5-flash-lite"
		} else {
			cfg.Model = "gemini-2.5-flash"
		}
	}
	if len(cfg.Fallback) == 0 {
		if cfg.Provider == "openai" {
			cfg.Fallback = []string{"nvidia/nemotron-3.5-lightning:free"}
		} else {
			cfg.Fallback = []string{"gemini-2.5-flash-lite"}
		}
	}
	if cfg.Port == 0 {
		cfg.Port = 8787
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 1024
	}
	if len(cfg.Models) == 0 {
		if cfg.Provider == "openai" {
			cfg.Models = []string{
				"google/gemini-2.5-flash-lite",
				"google/gemini-2.5-flash",
				"google/gemini-3.1-flash-lite",
				"google/gemini-3.5-flash-lite",
				"nvidia/nemotron-3.5-lightning:free",
				"nvidia/nemotron-3-super-120b-a12b:free",
				"google/gemma-4-31b-it:free",
			}
		} else {
			cfg.Models = append([]string{cfg.Model}, cfg.Fallback...)
		}
	}
	if cfg.System == "" {
		cfg.System = "你是一个运行在 Android 手机上的 AI agent，以 root 权限操作这台设备。始终用中文回复。\n\n" +
			"## 核心规则\n" +
			"1. 你不能靠「说」完成任何事。要打开东西、点按钮、输入文字、跑命令，必须调用对应工具。" +
			"没调用工具、没拿到工具结果，就不许说已完成。\n" +
			"2. 操作界面的标准循环：screen（看界面，拿到带坐标的节点）-> tap(该节点的 @x,@y) -> screen（确认界面变了）。" +
			"坐标只能用 screen 返回的值，不要自己估算。\n" +
			"3. 界面没变化就是操作失败：可能点偏了、被弹窗挡住、或页面还在加载。先 screen 看清楚，必要时 swipe 滚动找元素。\n\n" +
			"## 工具选择\n" +
			"- 打开 App -> open_app；打开文件/文件夹 -> open_path；打开网址 -> open_url\n" +
			"- 看界面 -> screen（首选，省 token）；界面是纯图形、screen 拿不到内容 -> screenshot\n" +
			"- 点击 -> tap；长按 -> tap 带 duration_ms>=400；滚动/翻页 -> swipe；返回/主页/回车 -> key\n" +
			"- 输入文字 -> text（会优先走 ADBKeyboard 以支持中文）\n" +
			"- 系统信息、批量文件、改设置 -> run_shell\n\n" +
			"## 中文输入的现实限制\n" +
			"Android 的 input text 只稳定支持 ASCII，中文要靠 ADBKeyboard。" +
			"如果 text 返回的提示说中文可能失败，就如实告诉用户结果并建议装 ADBKeyboard 或改用英文，不要假装成功。\n\n" +
			"## 危险操作\n" +
			"删除文件、卸载应用、改系统设置、发消息、付款这类不可逆或涉及用户数据的操作：" +
			"先在回复里说清楚你要做什么，等用户确认后再执行。\n\n" +
			"默认保存目录 /storage/emulated/0/Download，一律用绝对路径。\n\n" +
			"最后一行固定输出：以 'SARAN:' 开头，给 2-4 个后续动作，用 ' | ' 分隔。" +
			"例：SARAN: 打开微信 | 看看电池 | 清一下 Download"
	}
}

func saveConfig() {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	// 0600：配置里有 API key，别给别的应用读
	if err := os.WriteFile(configPath(), b, 0600); err != nil {
		log.Printf("config save error: %v", err)
	}
}

func setModel(m string) bool {
	m = strings.TrimSpace(m)
	if m == "" {
		return false
	}
	mu.Lock()
	cfg.Model = m
	mu.Unlock()
	saveConfig()
	log.Printf("[model] switched to %s", m)
	return true
}

// randomModel memilih model acak dari daftar (hindari model yang sama berturut-turut).
func randomModel() string {
	mu.Lock()
	list := append([]string(nil), cfg.Models...)
	cur := cfg.Model
	mu.Unlock()
	if len(list) == 0 {
		return cur
	}
	if len(list) == 1 {
		return list[0]
	}
	pick := list[rand.IntN(len(list))]
	for i := 0; i < 8 && pick == cur; i++ {
		pick = list[rand.IntN(len(list))]
	}
	return pick
}

// ==================== 会话管理 ====================
//
// 早期版本只有一条线性的 history.json。现在换成多会话：每条会话独立保存消息，
// 可切换、可归档、可删除，归档可以一键清空。旧的 history.json 在首次启动时
// 自动迁移成一条会话，不丢数据。

type Session struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Archived bool      `json:"archived"`
	Messages []Msg     `json:"messages"`
}

var (
	sessions []*Session
	activeID string
)

func historyPath() string  { return filepath.Join(home, "history.json") }
func sessionsPath() string { return filepath.Join(home, "sessions.json") }

func newSessionID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// activeSession 返回当前会话。sessions 里可能有 nil（JSON 里的 null），要跳过。
func activeSession() *Session {
	for _, s := range sessions {
		if s != nil && s.ID == activeID {
			return s
		}
	}
	return nil
}

func titleFromMessages(msgs []Msg) string {
	for _, m := range msgs {
		if m.Role == "user" {
			if t := strings.TrimSpace(m.Content); t != "" {
				return truncate(t, 24)
			}
		}
	}
	return "新会话"
}

func makeSession() *Session {
	now := time.Now()
	return &Session{ID: newSessionID(), Title: "新会话", Created: now, Updated: now}
}

// loadSessions 启动时调用（单线程，不需要锁）。
func loadSessions() {
	if b, err := os.ReadFile(sessionsPath()); err == nil {
		var st struct {
			Active   string     `json:"active"`
			Sessions []*Session `json:"sessions"`
		}
		if err := json.Unmarshal(b, &st); err == nil {
			sessions = st.Sessions
			activeID = st.Active
		}
	} else if old, e2 := os.ReadFile(historyPath()); e2 == nil {
		// 旧版单文件历史，迁移一次
		var msgs []Msg
		if json.Unmarshal(old, &msgs) == nil && len(msgs) > 0 {
			s := makeSession()
			s.Messages = msgs
			s.Title = titleFromMessages(msgs)
			sessions = []*Session{s}
			activeID = s.ID
			log.Printf("migrated legacy history.json (%d msgs) into session %s", len(msgs), s.ID)
		}
	}

	// 丢掉 nil 和空 ID，后面所有遍历就不用再判空
	cleaned := make([]*Session, 0, len(sessions))
	for _, s := range sessions {
		if s != nil && s.ID != "" {
			cleaned = append(cleaned, s)
		}
	}
	sessions = cleaned

	if len(sessions) == 0 {
		s := makeSession()
		sessions = []*Session{s}
		activeID = s.ID
	}
	if activeSession() == nil {
		activeID = sessions[0].ID
	}
	hist = append([]Msg(nil), activeSession().Messages...)
}

// saveSessions 落盘。调用方需持有 mu。
func saveSessions() {
	st := struct {
		Active   string     `json:"active"`
		Sessions []*Session `json:"sessions"`
	}{activeID, sessions}
	if b, err := json.MarshalIndent(st, "", " "); err == nil {
		os.WriteFile(sessionsPath(), b, 0600)
	}
}

// ensureActive 保证 activeID 指向一条存在且未归档的会话，并把 hist 换过去。
// 归档/删除之后必须调用：否则会出现「唯一的会话被归档了，但 activeID 仍指着它」
// 的僵局——未归档列表空着，界面却认为还有当前会话。
func ensureActive() {
	if s := activeSession(); s != nil && !s.Archived {
		return
	}
	for _, c := range sessions {
		if !c.Archived {
			activeID = c.ID
			hist = append([]Msg(nil), c.Messages...)
			return
		}
	}
	// 一条未归档的都不剩，建一条新的
	s := makeSession()
	sessions = append(sessions, s)
	activeID = s.ID
	hist = nil
}

// syncActive 把工作副本 hist 写回当前会话并落盘。调用方需持有 mu。
func syncActive() {
	if s := activeSession(); s != nil {
		s.Messages = append([]Msg(nil), hist...)
		s.Updated = time.Now()
		if len(s.Messages) > 0 {
			if t := titleFromMessages(s.Messages); t != "新会话" {
				s.Title = t
			}
		}
	}
	saveSessions()
}

func notify(title, text, url string) {
	if url == "" {
		return
	}
	// App 没装就直接放弃。否则 am start 会连试 30 次 × 4 秒把日志刷满
	// （模块还在但 app 被卸载是很常见的情况）。
	if out, err := exec.Command("pm", "list", "packages", "com.zafxyra.agent").Output(); err != nil ||
		!strings.Contains(string(out), "com.zafxyra.agent") {
		log.Printf("notify skipped: com.zafxyra.agent 未安装")
		return
	}
	// Jaga-jaga: pastikan app boleh posting notif (bisa ke-reset setelah reinstall)
	for _, g := range [][]string{
		{"pm", "grant", "com.zafxyra.agent", "android.permission.POST_NOTIFICATIONS"},
		{"appops", "set", "com.zafxyra.agent", "START_FOREGROUND", "allow"},
		{"appops", "set", "com.zafxyra.agent", "POST_NOTIFICATION", "allow"},
	} {
		exec.Command(g[0], g[1:]...).Run()
	}
	// Retry: PMS/user belum siap tepat setelah boot ('Activity class does not exist').
	// 次数收敛到 8 次、间隔递增，不再无限刷。
	for i := 0; i < 8; i++ {
		out, err := exec.Command("am", "start", "-n", "com.zafxyra.agent/.MainActivity",
			"--es", "url", url, "--ez", "boot", "true").CombinedOutput()
		s := string(out)
		if err == nil && !strings.Contains(s, "Error") && !strings.Contains(s, "does not exist") {
			return
		}
		log.Printf("am start attempt %d: %s", i+1, strings.TrimSpace(s))
		time.Sleep(time.Duration(2+i) * time.Second)
	}
}

func callLLM(msgs []Msg) (string, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return "", fmt.Errorf("还没配置 API Key。把它填到 %s 然后重启模块（或直接在聊天界面里设置）。", configPath())
	}
	if cfg.Provider == "openai" {
		return callOpenAI(msgs)
	}
	return callGemini(msgs)
}

type gemFn struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}
type gemFnResp struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}
type gemPart struct {
	Text             string     `json:"text,omitempty"`
	FunctionCall     *gemFn     `json:"functionCall,omitempty"`
	FunctionResponse *gemFnResp `json:"functionResponse,omitempty"`
}
type gemContent struct {
	Role  string    `json:"role"`
	Parts []gemPart `json:"parts"`
}

func doGemini(model string, contents []gemContent, sys string, tools []interface{}, force bool) (string, []gemFn, int, string, error) {
	reqBody := map[string]interface{}{"contents": contents}
	if sys != "" {
		reqBody["systemInstruction"] = map[string]interface{}{"parts": []gemPart{{Text: sys}}}
	}
	if len(tools) > 0 {
		reqBody["tools"] = tools
		if force {
			reqBody["toolConfig"] = map[string]interface{}{
				"functionCallingConfig": map[string]interface{}{"mode": "ANY"},
			}
		}
	}
	jb, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/models/%s:generateContent", strings.TrimRight(cfg.BaseURL, "/"), model)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jb))
	if err != nil {
		return "", nil, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", cfg.APIKey)
	resp, err := httpClient(120 * time.Second).Do(req)
	if err != nil {
		return "", nil, 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, resp.StatusCode, string(body), nil
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []gemPart `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", nil, resp.StatusCode, string(body), err
	}
	if len(out.Candidates) == 0 {
		return "", nil, resp.StatusCode, string(body), nil
	}
	var sb strings.Builder
	var calls []gemFn
	for _, p := range out.Candidates[0].Content.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
		if p.FunctionCall != nil {
			calls = append(calls, *p.FunctionCall)
		}
	}
	return sb.String(), calls, resp.StatusCode, "", nil
}

func pickModel() string {
	url := strings.TrimRight(cfg.BaseURL, "/") + "/models?pageSize=200"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("x-goog-api-key", cfg.APIKey)
	resp, err := httpClient(30 * time.Second).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	hasGen := func(methods []string) bool {
		for _, me := range methods {
			if me == "generateContent" {
				return true
			}
		}
		return false
	}
	for _, m := range out.Models {
		n := strings.TrimPrefix(m.Name, "models/")
		if hasGen(m.Methods) && strings.Contains(n, "flash") && !strings.Contains(n, "exp") && !strings.Contains(n, "thinking") {
			return n
		}
	}
	for _, m := range out.Models {
		if hasGen(m.Methods) {
			return strings.TrimPrefix(m.Name, "models/")
		}
	}
	return ""
}

// maxAgentSteps 是一次请求内最多执行多少轮工具调用。
// 原来是 8，复杂任务（例如统计几十个 App 的通知）走到一半就被截断，
// 模型没有机会输出正文，界面只剩下兜底文案，看起来像"AI 中断了"。
const maxAgentSteps = 24

func geminiAgent(model string, msgs []Msg) (string, int, string, error) {
	contents := []gemContent{}
	var sys string
	for _, m := range msgs {
		switch m.Role {
		case "system":
			sys = m.Content
		case "assistant":
			contents = append(contents, gemContent{Role: "model", Parts: []gemPart{{Text: m.Content}}})
		default:
			contents = append(contents, gemContent{Role: "user", Parts: []gemPart{{Text: m.Content}}})
		}
	}
	tools := toolDecls()
	force := looksLikeCommand(lastUserMsg(msgs))
	for step := 0; step < maxAgentSteps; step++ {
		text, calls, status, body, err := doGemini(model, contents, sys, tools, force && step == 0)
		if err != nil {
			return "", 0, "", err
		}
		if status < 200 || status >= 300 {
			return "", status, body, nil
		}
		if len(calls) == 0 {
			return text, status, "", nil
		}
		// Langkah agent: eksekusi tool, kirim hasilnya balik ke model, ulangi.
		var cparts []gemPart
		var rparts []gemPart
		for i := range calls {
			c := calls[i]
			cparts = append(cparts, gemPart{FunctionCall: &c})
			out := execTool(c.Name, c.Args)
			rparts = append(rparts, gemPart{FunctionResponse: &gemFnResp{Name: c.Name, Response: map[string]interface{}{"result": out}}})
		}
		contents = append(contents, gemContent{Role: "model", Parts: cparts})
		contents = append(contents, gemContent{Role: "user", Parts: rparts})
	}
	return "我已经连续执行了 24 步，达到单次上限。任务可能还没做完 —— " +
		"直接说「继续」我就接着往下做，或者把范围缩小一点（比如一次只处理一个 App）。", 200, "", nil
}

func callGemini(msgs []Msg) (string, error) {
	text, status, body, err := geminiAgent(cfg.Model, msgs)
	if err != nil {
		return "", err
	}
	if status >= 200 && status < 300 {
		if text != "" {
			return text, nil
		}
		return "", fmt.Errorf("empty response: %s", body)
	}
	if status == 404 {
		if pm := pickModel(); pm != "" && pm != cfg.Model {
			log.Printf("model %s unavailable, retrying with %s", cfg.Model, pm)
			t2, s2, b2, e2 := geminiAgent(pm, msgs)
			if e2 != nil {
				return "", e2
			}
			if s2 >= 200 && s2 < 300 && t2 != "" {
				return t2, nil
			}
			body = b2
		}
	}
	return "", fmt.Errorf("HTTP %d: %s", status, body)
}

func toolDecls() []interface{} {
	funcs := []interface{}{
		map[string]interface{}{
			"name":        "open_app",
			"description": "按名称打开手机上的应用（如 微信、网易云音乐），也可以直接给包名（如 com.tencent.mm）。",
			"parameters": map[string]interface{}{
				"type": "OBJECT",
				"properties": map[string]interface{}{
					"app": map[string]interface{}{"type": "STRING", "description": "应用名或包名"},
				},
				"required": []string{"app"},
			},
		},
		map[string]interface{}{
			"name":        "open_url",
			"description": "在手机浏览器里打开一个网址。如果没有写协议，自动补 https://。",
			"parameters": map[string]interface{}{
				"type": "OBJECT",
				"properties": map[string]interface{}{
					"url": map[string]interface{}{"type": "STRING", "description": "要打开的网址"},
				},
				"required": []string{"url"},
			},
		},
		map[string]interface{}{
			"name":        "web_search",
			"description": "用 Google 搜索，并在浏览器里打开结果页。",
			"parameters": map[string]interface{}{
				"type": "OBJECT",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "STRING", "description": "搜索关键词"},
				},
				"required": []string{"query"},
			},
		},
	}
	if shellAllowed() {
		funcs = append(funcs, map[string]interface{}{
			"name": "run_shell",
			"description": "以 root 身份在手机上执行 shell 命令并返回输出。" +
				"用于查看系统信息、批量处理文件、修改设置、读取通知库等。" +
				"例如：'df -h'、'ls -l /sdcard/Download'、'dumpsys battery'、'pm list packages | grep wechat'。",
			"parameters": map[string]interface{}{
				"type": "OBJECT",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "STRING", "description": "要通过 sh -c 执行的命令"},
				},
				"required": []string{"command"},
			},
		})
	}
	gemStr := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "STRING", "description": desc}
	}
	gemTool := func(name, desc string, props map[string]interface{}, required []string) map[string]interface{} {
		return map[string]interface{}{
			"name": name, "description": desc,
			"parameters": map[string]interface{}{"type": "OBJECT", "properties": props, "required": required},
		}
	}
	funcs = append(funcs,
		gemTool("write_file", "写入或新建文件（已存在则覆盖），父目录会自动创建。必须用绝对路径，如 /storage/emulated/0/Download/note.txt。", map[string]interface{}{"path": gemStr("文件的绝对路径"), "content": gemStr("文件内容")}, []string{"path", "content"}),
		gemTool("read_file", "读取一个文本文件的内容。使用绝对路径。", map[string]interface{}{"path": gemStr("文件的绝对路径")}, []string{"path"}),
		gemTool("list_dir", "列出某个目录下的文件和文件夹。", map[string]interface{}{"path": gemStr("目录路径，如 /storage/emulated/0/Download")}, []string{"path"}),
		gemTool("make_dir", "创建目录（父目录会一并创建）。", map[string]interface{}{"path": gemStr("目录路径")}, []string{"path"}),
		gemTool("move_file", "移动或重命名文件/文件夹。", map[string]interface{}{"from": gemStr("源路径"), "to": gemStr("目标路径")}, []string{"from", "to"}),
		gemTool("delete_path", "删除文件或文件夹。", map[string]interface{}{"path": gemStr("要删除的路径")}, []string{"path"}),
		gemTool("download_url", "从网络下载文件并保存到手机上的指定路径。", map[string]interface{}{"url": gemStr("要下载的文件地址"), "path": gemStr("保存路径")}, []string{"url", "path"}),
		gemTool("open_path", "用系统的默认应用打开一个文件或文件夹。适用于「打开 catatan.txt」「打开 Download 文件夹」「打开这张图片」这类请求。注意：打开应用请用 open_app，不要用这个。", map[string]interface{}{"path": gemStr("文件或目录的绝对路径，如 /storage/emulated/0/Download/catatan.txt")}, []string{"path"}),
		gemTool("schedule_message", "安排一条定时提醒：到点后 agent 会主动把这条消息发给你。当用户说「10 分钟后提醒我吃药」时使用。", map[string]interface{}{"delay_seconds": map[string]interface{}{"type": "INTEGER", "description": "多少秒后发送（延迟秒数）"}, "message": gemStr("到点要发送的提醒内容")}, []string{"delay_seconds", "message"}),
		// ---- UI 自动化：让 agent 真正能操作界面，而不只是开关 App ----
		gemTool("screen", "读取当前界面结构，返回带中心坐标的可点节点列表。需要看界面、找按钮、确认上一步操作是否生效时，优先用这个，比截图省大量 token。", map[string]interface{}{}, []string{}),
		gemTool("tap", "点击屏幕坐标。坐标必须来自 screen 返回的 @(x,y)，不要自己估算。duration_ms>=400 视为长按。", map[string]interface{}{"x": map[string]interface{}{"type": "INTEGER", "description": "横坐标"}, "y": map[string]interface{}{"type": "INTEGER", "description": "纵坐标"}, "duration_ms": map[string]interface{}{"type": "INTEGER", "description": "按住时长毫秒，默认 80；>=400 为长按"}}, []string{"x", "y"}),
		gemTool("swipe", "在屏幕上滑动，用于滚动列表、翻页、下拉通知栏。", map[string]interface{}{"x1": map[string]interface{}{"type": "INTEGER", "description": "起点横坐标"}, "y1": map[string]interface{}{"type": "INTEGER", "description": "起点纵坐标"}, "x2": map[string]interface{}{"type": "INTEGER", "description": "终点横坐标"}, "y2": map[string]interface{}{"type": "INTEGER", "description": "终点纵坐标"}, "duration_ms": map[string]interface{}{"type": "INTEGER", "description": "滑动时长毫秒，默认 300"}}, []string{"x1", "y1", "x2", "y2"}),
		gemTool("key", "发送按键事件，用于返回、回到主页、回车、删除等。", map[string]interface{}{"key": gemStr("HOME/BACK/ENTER/POWER/TAB/DEL/MENU/RECENT/VOLUP/VOLDOWN，或数字 keycode")}, []string{"key"}),
		gemTool("text", "向当前聚焦的输入框输入文字。会优先走 ADBKeyboard 以正确支持中文；没装则降级为 input text（中文可能丢失）。", map[string]interface{}{"text": gemStr("要输入的文字")}, []string{"text"}),
		gemTool("screenshot", "截屏保存为 PNG 文件并返回路径。只在 screen 看不出内容（纯图形界面）时才用。", map[string]interface{}{"path": gemStr("保存路径，留空则存到 /storage/emulated/0/Download")}, []string{}),
	)
	return []interface{}{
		map[string]interface{}{"functionDeclarations": funcs},
	}
}

var appAliases = map[string]string{
	"whatsapp": "com.whatsapp", "wa": "com.whatsapp",
	"youtube": "com.google.android.youtube", "yt": "com.google.android.youtube",
	"chrome": "com.android.chrome", "browser": "com.android.chrome",
	"instagram": "com.instagram.android", "ig": "com.instagram.android",
	"tiktok":   "com.zhiliaoapp.musically",
	"telegram": "org.telegram.messenger",
	"facebook": "com.facebook.katana", "fb": "com.facebook.katana",
	"gmail":     "com.google.android.gm",
	"maps":      "com.google.android.apps.maps",
	"spotify":   "com.spotify.music",
	"netflix":   "com.netflix.mediaclient",
	"shopee":    "com.shopee.id",
	"tokopedia": "com.tokopedia.tkpd",
	"gojek":     "com.gojek.app",
	"grab":      "com.grabtaxi.passenger",
	"dana":      "id.dana", "ovo": "id.co.ovo",
	"settings": "com.android.settings", "pengaturan": "com.android.settings",
	"kalkulator": "com.coloros.calculator", "calculator": "com.coloros.calculator",
}

var (
	pkgCache []string
	pkgMu    sync.Mutex
)

func installedPackages() []string {
	pkgMu.Lock()
	defer pkgMu.Unlock()
	if pkgCache != nil {
		return pkgCache
	}
	out, _ := exec.Command("pm", "list", "packages").Output()
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "package:"))
		if l != "" {
			pkgCache = append(pkgCache, l)
		}
	}
	return pkgCache
}

func refreshPackages() {
	pkgMu.Lock()
	pkgCache = nil
	pkgMu.Unlock()
	installedPackages()
}

func isInstalled(pkg string) bool {
	for _, p := range installedPackages() {
		if p == pkg {
			return true
		}
	}
	return false
}

func resolvePackage(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, pre := range []string{"aplikasi ", "app ", "the ", "buka "} {
		n = strings.TrimPrefix(n, pre)
	}
	n = strings.TrimSuffix(n, " app")
	if n == "" {
		return ""
	}
	if strings.Contains(n, ".") && !strings.ContainsAny(n, " ") {
		return n
	}
	if p, ok := appAliases[n]; ok && isInstalled(p) {
		return p
	}
	find := func(list []string) string {
		var best string
		for _, p := range list {
			short := p
			if i := strings.LastIndex(p, "."); i >= 0 {
				short = p[i+1:]
			}
			if short == n || strings.Contains(short, n) || strings.Contains(p, n) {
				if best == "" || len(p) < len(best) {
					best = p
				}
			}
		}
		return best
	}
	if p := find(installedPackages()); p != "" {
		return p
	}
	refreshPackages()
	return find(installedPackages())
}

// launcherComponent mencari activity launcher utama sebuah package.
func launcherComponent(pkg string) string {
	out, _ := exec.Command("cmd", "package", "resolve-activity", "--brief",
		"-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", pkg).Output()
	comp := ""
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "/") && !strings.Contains(l, " ") {
			comp = l
		}
	}
	return comp
}

// focused melaporkan apakah sebuah package sedang menjadi window fokus.
func focused(pkg string) bool {
	if pkg == "" {
		return false
	}
	out, _ := exec.Command("sh", "-c", "dumpsys window 2>/dev/null | grep mCurrentFocus").CombinedOutput()
	return strings.Contains(string(out), pkg)
}

func openApp(name string) (string, error) {
	defer noteAction("open_app")
	pkg := resolvePackage(name)
	if pkg == "" {
		return "", fmt.Errorf("aplikasi \"%s\" 没有安装或不存在", name)
	}
	// Cara utama: resolve komponen launcher lalu 'am start -W'. Output sistem
	// ("Status: ok" / "...brought to the front") menjadi acuan keberhasilan.
	if comp := launcherComponent(pkg); comp != "" {
		out, err := exec.Command("am", "start", "-W", "-n", comp).CombinedOutput()
		s := string(out)
		log.Printf("am start -W -n %s => err=%v out=%q", comp, err, strings.TrimSpace(s))
		if err == nil && strings.Contains(s, "Status: ok") &&
			!strings.Contains(s, "Error") && !strings.Contains(s, "does not exist") &&
			!strings.Contains(s, "not found") {
			log.Printf("openApp %s: dibawa ke depan (focused=%v)", pkg, focused(pkg))
			return fmt.Sprintf("好，已打开 %s。", pkg), nil
		}
	}
	// Fallback: monkey.
	out, err := exec.Command("monkey", "-p", pkg, "-c", "android.intent.category.LAUNCHER", "1").CombinedOutput()
	s := string(out)
	log.Printf("monkey -p %s => err=%v out=%q", pkg, err, strings.TrimSpace(s))
	if err == nil && !strings.Contains(s, "No activities found") && !strings.Contains(s, "aborted") && !strings.Contains(s, "Error") {
		return fmt.Sprintf("好，已打开 %s。", pkg), nil
	}
	return "", fmt.Errorf("gagal buka %s: %s", pkg, strings.TrimSpace(s))
}

func openURL(u string) (string, error) {
	defer noteAction("open_url")
	u = strings.TrimSpace(u)
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	out, err := exec.Command("am", "start", "-a", "android.intent.action.VIEW", "-d", u).CombinedOutput()
	s := string(out)
	if err != nil || strings.Contains(s, "Error") || strings.Contains(s, "does not exist") {
		return "", fmt.Errorf("gagal buka URL %s: %s", u, strings.TrimSpace(s))
	}
	return fmt.Sprintf("Oke, aku buka %s sekarang.", u), nil
}

// commonDirs memetakan nama folder umum -> path absolut.
var commonDirs = map[string]string{
	"download":  "/storage/emulated/0/Download",
	"downloads": "/storage/emulated/0/Download",
	"unduhan":   "/storage/emulated/0/Download",
	"document":  "/storage/emulated/0/Documents",
	"documents": "/storage/emulated/0/Documents",
	"dokumen":   "/storage/emulated/0/Documents",
	"dcim":      "/storage/emulated/0/DCIM",
	"picture":   "/storage/emulated/0/Pictures",
	"pictures":  "/storage/emulated/0/Pictures",
	"gambar":    "/storage/emulated/0/Pictures",
	"photo":     "/storage/emulated/0/DCIM/Camera",
	"foto":      "/storage/emulated/0/DCIM/Camera",
	"music":     "/storage/emulated/0/Music",
	"musik":     "/storage/emulated/0/Music",
	"movie":     "/storage/emulated/0/Movies",
	"movies":    "/storage/emulated/0/Movies",
	"video":     "/storage/emulated/0/Movies",
	"android":   "/storage/emulated/0/Android",
	"whatsapp":  "/storage/emulated/0/Android/media/com.whatsapp/WhatsApp",
	"telegram":  "/storage/emulated/0/Telegram",
}

var fileExtMime = map[string]string{
	"txt": "text/plain", "md": "text/plain", "log": "text/plain", "json": "application/json",
	"csv": "text/csv", "xml": "text/xml", "html": "text/html", "htm": "text/html",
	"pdf": "application/pdf",
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"webp": "image/webp", "bmp": "image/bmp", "svg": "image/svg+xml", "heic": "image/heic",
	"mp3": "audio/mpeg", "wav": "audio/x-wav", "m4a": "audio/mp4", "ogg": "audio/ogg",
	"flac": "audio/flac", "aac": "audio/aac",
	"mp4": "video/mp4", "mkv": "video/x-matroska", "avi": "video/x-msvideo",
	"webm": "video/webm", "3gp": "video/3gpp", "mov": "video/quicktime",
	"apk": "application/vnd.android.package-archive", "zip": "application/zip",
	"rar": "application/vnd.rar", "7z": "application/x-7z-compressed",
	"doc": "application/msword", "docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls": "application/vnd.ms-excel", "xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"ppt": "application/vnd.ms-powerpoint", "pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

func fileExt(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if i := strings.LastIndex(base, "."); i >= 0 && i < len(base)-1 {
		return base[i+1:]
	}
	return ""
}

func hasFileExt(s string) bool {
	e := fileExt(s)
	if e == "" || len(e) > 5 {
		return false
	}
	_, ok := fileExtMime[e]
	return ok
}

func mimeFor(path string) string {
	if m, ok := fileExtMime[fileExt(path)]; ok {
		return m
	}
	return "*/*"
}

// expandPath mengubah nama pendek (mis. "Download", "catatan.txt") menjadi path absolut.
func expandPath(p string) string {
	p = strings.TrimSpace(strings.Trim(p, "\"'"))
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~/") {
		p = "/storage/emulated/0/" + p[2:]
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if abs, ok := commonDirs[strings.ToLower(strings.TrimSuffix(p, "/"))]; ok {
		return abs
	}
	for _, dir := range []string{"/storage/emulated/0", "/storage/emulated/0/Download", "/storage/emulated/0/Documents", "/storage/emulated/0/Pictures", "/storage/emulated/0/DCIM", "/storage/emulated/0/Music", "/storage/emulated/0/Movies"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			return filepath.Join(dir, p)
		}
	}
	return "/storage/emulated/0/" + p
}

// looksLikePath menebak apakah argumen merujuk ke file/folder (bukan aplikasi).
func looksLikePath(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	if l == "" {
		return false
	}
	if strings.HasPrefix(l, "/") || strings.HasPrefix(l, "~/") || strings.Contains(l, "/") {
		return true
	}
	if hasFileExt(l) {
		return true
	}
	if _, ok := commonDirs[l]; ok {
		return true
	}
	for _, kw := range []string{"folder", "file", "direktori", "galeri", "gallery"} {
		if strings.HasPrefix(l, kw+" ") {
			return true
		}
	}
	return false
}

// openPath membuka file/folder dengan aplikasi penampil/pengelola bawaan.
func openPath(path string) (string, error) {
	defer noteAction("open_path")
	p := expandPath(path)
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("找不到: %s", p)
	}
	if fi.IsDir() {
		out, e := exec.Command("am", "start", "-a", "android.intent.action.VIEW",
			"-d", "file://"+p, "-t", "resource/folder").CombinedOutput()
		s := string(out)
		log.Printf("openPath dir %s => err=%v out=%q", p, e, strings.TrimSpace(s))
		if e == nil && !strings.Contains(s, "Error") && !strings.Contains(s, "does not exist") {
			return "好，已打开文件夹：" + p, nil
		}
		return listDir(p)
	}
	mime := mimeFor(p)
	out, e := exec.Command("am", "start", "-a", "android.intent.action.VIEW",
		"-d", "file://"+p, "-t", mime).CombinedOutput()
	s := string(out)
	log.Printf("openPath file %s mime=%s => err=%v out=%q", p, mime, e, strings.TrimSpace(s))
	if e != nil || strings.Contains(s, "Error") || strings.Contains(s, "does not exist") {
		return "", fmt.Errorf("gagal membuka %s: %s", p, strings.TrimSpace(s))
	}
	return "好，已打开文件：" + p, nil
}

func webSearch(q string) (string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", fmt.Errorf("kata kunci kosong")
	}
	return openURL("https://www.google.com/search?q=" + strings.ReplaceAll(q, " ", "+"))
}

func shellAllowed() bool {
	return cfg.AllowShell == nil || *cfg.AllowShell
}

func runShell(cmd string) (string, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", fmt.Errorf("命令为空")
	}
	if !shellAllowed() {
		return "", fmt.Errorf("akses shell dimatikan (set \"allow_shell\": true di config.json)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	out, err := c.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if len(s) > 4000 {
		s = s[:4000] + "\n…(output dipotong)"
	}
	if ctx.Err() == context.DeadlineExceeded {
		if s == "" {
			s = "（无输出）"
		}
		return s + "\n(timeout 60s)", nil
	}
	if err != nil && s == "" {
		return "", fmt.Errorf("gagal: %v", err)
	}
	if s == "" {
		s = "（命令执行完成，无输出）"
	}
	return s, nil
}

// ==================== UI 自动化 ====================
//
// 这一组工具让 agent 能"看见"并"操作"界面，而不只是开关 App。
// 全部基于 Android 自带的 uiautomator / input / screencap，零第三方依赖。
//
// 设计取舍：
//   - 用 uiautomator dump 拿 UI 树而不是截图，token 便宜一个数量级，
//     而且能直接拿到可点坐标，模型不用"看图猜位置"。
//   - 所有坐标都从 bounds 算中心点，模型只需照抄，降低点偏的概率。

const uiDumpPath = "/data/local/tmp/.agentd_ui.xml"

type uiNodeXML struct {
	Text       string      `xml:"text,attr"`
	Desc       string      `xml:"content-desc,attr"`
	Class      string      `xml:"class,attr"`
	Package    string      `xml:"package,attr"`
	Clickable  string      `xml:"clickable,attr"`
	Scrollable string      `xml:"scrollable,attr"`
	Enabled    string      `xml:"enabled,attr"`
	Bounds     string      `xml:"bounds,attr"`
	Nodes      []uiNodeXML `xml:"node"`
}

// parseBounds 解析 uiautomator 的坐标串 "[x1,y1][x2,y2]"。
// 解析不完整时返回全零 + false，避免调用方误用到半截坐标。
func parseBounds(s string) (x1, y1, x2, y2 int, ok bool) {
	var a, b, c, d int
	n, err := fmt.Sscanf(s, "[%d,%d][%d,%d]", &a, &b, &c, &d)
	if err != nil || n != 4 {
		return 0, 0, 0, 0, false
	}
	return a, b, c, d, true
}

// currentWindow 取当前焦点窗口。纯辅助信息，失败就当没有。
func currentWindow() string {
	out, err := exec.Command("sh", "-c",
		"dumpsys window 2>/dev/null | grep -E 'mCurrentFocus|mFocusedApp' | head -2").CombinedOutput()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// uiDumpXML 导出 UI XML。优先 --compressed（实测体积降 60%），不支持则回退。
func uiDumpXML() ([]byte, error) {
	_ = exec.Command("sh", "-c", "rm -f "+uiDumpPath).Run()

	try := func(flagArg string) bool {
		_ = exec.Command("sh", "-c",
			"uiautomator dump "+flagArg+" "+uiDumpPath+" >/dev/null 2>&1").Run()
		st, err := os.Stat(uiDumpPath)
		return err == nil && st.Size() > 0
	}
	if !try("--compressed") && !try("") {
		return nil, fmt.Errorf("uiautomator dump 失败：界面可能是 FLAG_SECURE，或系统的自动化服务被限制")
	}
	data, err := os.ReadFile(uiDumpPath)
	_ = os.Remove(uiDumpPath)
	if err != nil {
		return nil, fmt.Errorf("读取 dump 结果失败: %v", err)
	}
	return data, nil
}

// parseUILines 把 uiautomator 的 XML 拍平成「文字 + 中心坐标」的行。
// 单独抽出来是为了能脱离设备做单元测试。
func parseUILines(data []byte) ([]string, error) {
	var root uiNodeXML
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("解析 UI XML 失败: %v", err)
	}

	var lines []string
	var walk func(n uiNodeXML, depth int)
	walk = func(n uiNodeXML, depth int) {
		label := strings.TrimSpace(n.Text)
		if label == "" {
			label = strings.TrimSpace(n.Desc)
		}
		if label != "" && depth < 48 {
			if x1, y1, x2, y2, ok := parseBounds(n.Bounds); ok {
				cls := n.Class
				if i := strings.LastIndex(cls, "."); i >= 0 {
					cls = cls[i+1:]
				}
				var flags []string
				if n.Clickable == "true" {
					flags = append(flags, "可点")
				}
				if n.Scrollable == "true" {
					flags = append(flags, "可滚")
				}
				if n.Enabled == "false" {
					flags = append(flags, "禁用")
				}
				label = strings.ReplaceAll(strings.ReplaceAll(label, "\n", " "), "\r", "")
				lines = append(lines, fmt.Sprintf("[%d] %s @(%d,%d) %s %s",
					len(lines)+1, truncate(label, 60), (x1+x2)/2, (y1+y2)/2,
					cls, strings.Join(flags, " ")))
			}
		}
		for _, c := range n.Nodes {
			walk(c, depth+1)
		}
	}
	walk(root, 0)
	return lines, nil
}

// screenTool 返回紧凑的界面结构：每个带文字的节点一行，附中心坐标。
func screenTool() (string, error) {
	data, err := uiDumpXML()
	if err != nil {
		return "", err
	}
	lines, err := parseUILines(data)
	if err != nil {
		return "", err
	}

	win := currentWindow()
	head := ""
	if win != "" {
		head = "当前窗口: " + win + "\n\n"
	}
	if len(lines) == 0 {
		return head + "(界面里没有带文字的可点节点：可能是 FLAG_SECURE 界面，或纯图形绘制)", nil
	}
	if len(lines) > 90 {
		total := len(lines)
		lines = append(lines[:90], fmt.Sprintf("…还有 %d 个节点未显示", total-90))
	}
	return head + strings.Join(lines, "\n"), nil
}

func tapTool(x, y, durMs int) (string, error) {
	if x < 0 || y < 0 || x > 20000 || y > 20000 {
		return "", fmt.Errorf("坐标不合理: (%d,%d)", x, y)
	}
	var cmd string
	if durMs >= 400 {
		cmd = fmt.Sprintf("input swipe %d %d %d %d %d", x, y, x, y, durMs)
	} else {
		cmd = fmt.Sprintf("input tap %d %d", x, y)
	}
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		return "", fmt.Errorf("点击失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if durMs >= 400 {
		return fmt.Sprintf("已长按 (%d,%d) %dms", x, y, durMs), nil
	}
	return fmt.Sprintf("已点击 (%d,%d)", x, y), nil
}

func swipeTool(x1, y1, x2, y2, durMs int) (string, error) {
	if durMs <= 0 {
		durMs = 300
	}
	cmd := fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, durMs)
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		return "", fmt.Errorf("滑动失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("已滑动 (%d,%d) -> (%d,%d) %dms", x1, y1, x2, y2, durMs), nil
}

var keyNames = map[string]string{
	"HOME": "3", "BACK": "4", "ENTER": "66", "POWER": "26", "TAB": "61",
	"DEL": "67", "BACKSPACE": "67", "MENU": "82", "SEARCH": "84",
	"RECENT": "187", "APP_SWITCH": "187", "SPACE": "62", "CAMERA": "27",
	"WAKEUP": "224", "SLEEP": "223", "MUTE": "164", "PLAYPAUSE": "85",
	"VOLUP": "24", "VOLDOWN": "25", "UP": "19", "DOWN": "20",
	"LEFT": "21", "RIGHT": "22", "CENTER": "23",
}

func keyTool(k string) (string, error) {
	k = strings.TrimSpace(k)
	if k == "" {
		return "", fmt.Errorf("缺少按键名")
	}
	code := k
	if v, ok := keyNames[strings.ToUpper(k)]; ok {
		code = v
	}
	// 只接受纯数字，杜绝把用户输入拼进 shell
	if _, err := strconv.Atoi(code); err != nil {
		return "", fmt.Errorf("未知按键: %s（可用 HOME/BACK/ENTER/POWER/TAB/或数字 keycode）", k)
	}
	if out, err := exec.Command("sh", "-c", "input keyevent "+code).CombinedOutput(); err != nil {
		return "", fmt.Errorf("按键失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return "已按键 " + strings.ToUpper(k) + " (" + code + ")", nil
}

var (
	adbKbOnce sync.Once
	adbKbOK   bool
)

// hasADBKeyboard 检测有没有装 ADBKeyboard（能可靠输入任意 UTF-8 的输入法）。
func hasADBKeyboard() bool {
	adbKbOnce.Do(func() {
		out, _ := exec.Command("sh", "-c",
			"pm list packages 2>/dev/null | grep -q com.android.adbkeyboard && echo yes").CombinedOutput()
		adbKbOK = strings.TrimSpace(string(out)) == "yes"
	})
	return adbKbOK
}

// textTool 向当前焦点输入文字。
//
// 中文是个老问题：Android 的 `input text` 只可靠支持 ASCII。
// 三级降级：ADBKeyboard 广播（最稳）-> input text（部分 ROM 吃 UTF-8）-> 明确报错提示装 ADBKeyboard。
func textTool(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("要输入的文字为空")
	}
	ascii := true
	for _, r := range s {
		if r < 32 || r > 126 {
			ascii = false
			break
		}
	}

	if hasADBKeyboard() {
		q := strings.ReplaceAll(s, `"`, `\"`)
		cmd := `am broadcast -a ADB_INPUT_TEXT --es msg "` + q + `"`
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err == nil {
			_ = out
			return "已输入（ADBKeyboard 广播通道）", nil
		}
	}

	// shell 单引号内转义：' -> '\''
	q := strings.ReplaceAll(s, "'", `'\''`)
	out, err := exec.Command("sh", "-c", "input text '"+q+"'").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("输入失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if !ascii {
		return "已尝试输入（含非 ASCII 字符）。input text 对中文支持有限，" +
			"如果输入框里没出现文字，装 ADBKeyboard 后重试，或改用英文。", nil
	}
	return "已输入", nil
}

func screenshotTool(savePath string) (string, error) {
	if strings.TrimSpace(savePath) == "" {
		savePath = "/storage/emulated/0/Download/shot_" +
			time.Now().Format("20060102_150405") + ".png"
	}
	savePath = expandPath(savePath)

	tmp := "/data/local/tmp/.agentd_shot.png"
	_ = exec.Command("sh", "-c", "rm -f "+tmp).Run()
	if out, err := exec.Command("sh", "-c", "screencap -p "+tmp).CombinedOutput(); err != nil {
		return "", fmt.Errorf("screencap 失败: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	st, err := os.Stat(tmp)
	if err != nil || st.Size() == 0 {
		return "", fmt.Errorf("screencap 没有产生文件（界面可能是 FLAG_SECURE）")
	}
	if out, err := exec.Command("sh", "-c",
		"cp "+tmp+" "+shellQuote(savePath)+" && chmod 0666 "+shellQuote(savePath)).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("保存到 %s 失败: %v (%s)", savePath, err, strings.TrimSpace(string(out)))
	}
	_ = os.Remove(tmp)
	return fmt.Sprintf("截图已保存: %s (%d KB)", savePath, st.Size()/1024), nil
}

func resultOrErr(msg string, err error) string {
	if err != nil {
		return err.Error()
	}
	return msg
}

func argStr(args map[string]interface{}, k string) string {
	if v, ok := args[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func toolStatus(name string, args map[string]interface{}) string {
	switch name {
	case "open_app":
		if a := argStr(args, "app"); a != "" {
			return "打开 " + a + "…"
		}
		return "打开应用…"
	case "open_url":
		return "打开链接…"
	case "web_search":
		if q := argStr(args, "query"); q != "" {
			return "搜索「" + q + "」…"
		}
		return "搜索网页…"
	case "run_shell":
		return "执行命令…"
	case "write_file":
		return "写入文件…"
	case "read_file":
		return "读取文件…"
	case "list_dir":
		return "查看目录…"
	case "make_dir":
		return "创建目录…"
	case "move_file":
		return "移动/重命名…"
	case "delete_path":
		return "删除中…"
	case "download_url":
		return "下载文件…"
	case "open_path":
		if p := argStr(args, "path"); p != "" {
			return "打开 " + p + "…"
		}
		return "打开文件/目录…"
	case "schedule_message":
		if d := int(numArg(args, "delay_seconds")); d > 0 {
			return fmt.Sprintf("设置提醒（%d 秒后）…", d)
		}
		return "设置提醒…"
	// ---- UI 自动化 ----
	case "screen":
		return "读取屏幕…"
	case "tap":
		if x, y := int(numArg(args, "x")), int(numArg(args, "y")); x > 0 || y > 0 {
			return fmt.Sprintf("点击 (%d,%d)…", x, y)
		}
		return "点击屏幕…"
	case "swipe":
		return "滑动屏幕…"
	case "key":
		if k := argStr(args, "key"); k != "" {
			return "按键 " + strings.ToUpper(k) + "…"
		}
		return "按键…"
	case "text":
		if t := argStr(args, "text"); t != "" {
			return "输入「" + truncate(t, 20) + "」…"
		}
		return "输入文字…"
	case "screenshot":
		return "截屏…"
	}
	return ""
}

// execTool menjalankan satu aksi tool dan mengembalikan hasil (atau pesan error).
// execTool menjalankan satu aksi tool sambil memancarkan event 'tool' (mulai + hasil).
func execTool(name string, args map[string]interface{}) string {
	noteAction(name)
	id := nextToolID()
	label := toolStatus(name, args)
	if label == "" {
		label = name
	}
	broadcast(event{Type: "tool", ID: id, Name: name, Label: label, State: "start"})
	out := execToolInner(name, args)
	broadcast(event{Type: "tool", ID: id, Name: name, Label: label, Result: truncate(out, 500), State: "done"})
	return out
}

func execToolInner(name string, args map[string]interface{}) string {
	str := func(k string) string {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	switch name {
	case "open_app":
		log.Printf("[tool] open_app app=%q", str("app"))
		return resultOrErr(openApp(str("app")))
	case "open_url":
		log.Printf("[tool] open_url url=%q", str("url"))
		return resultOrErr(openURL(str("url")))
	case "web_search":
		q := str("query")
		if q == "" {
			q = str("q")
		}
		log.Printf("[tool] web_search q=%q", q)
		return resultOrErr(webSearch(q))
	case "run_shell":
		log.Printf("[tool] run_shell cmd=%q", str("command"))
		return resultOrErr(runShell(str("command")))
	case "write_file":
		return resultOrErr(writeFile(str("path"), str("content")))
	case "read_file":
		return resultOrErr(readFile(str("path")))
	case "list_dir":
		return resultOrErr(listDir(str("path")))
	case "make_dir":
		return resultOrErr(makeDir(str("path")))
	case "move_file":
		return resultOrErr(moveFile(str("from"), str("to")))
	case "delete_path":
		return resultOrErr(deletePath(str("path")))
	case "download_url":
		return resultOrErr(downloadURL(str("url"), str("path")))
	case "open_path":
		log.Printf("[tool] open_path path=%q", str("path"))
		return resultOrErr(openPath(str("path")))
	case "schedule_message":
		delay := int(numArg(args, "delay_seconds"))
		msg := str("message")
		log.Printf("[tool] schedule_message delay=%ds msg=%q", delay, msg)
		return scheduleMessage(delay, msg)
	case "screen":
		log.Printf("[tool] screen")
		return resultOrErr(screenTool())
	case "tap":
		x, y := int(numArg(args, "x")), int(numArg(args, "y"))
		log.Printf("[tool] tap (%d,%d)", x, y)
		return resultOrErr(tapTool(x, y, int(numArg(args, "duration_ms"))))
	case "swipe":
		log.Printf("[tool] swipe")
		return resultOrErr(swipeTool(
			int(numArg(args, "x1")), int(numArg(args, "y1")),
			int(numArg(args, "x2")), int(numArg(args, "y2")),
			int(numArg(args, "duration_ms"))))
	case "key":
		log.Printf("[tool] key %q", str("key"))
		return resultOrErr(keyTool(str("key")))
	case "text":
		log.Printf("[tool] text %q", truncate(str("text"), 40))
		return resultOrErr(textTool(str("text")))
	case "screenshot":
		log.Printf("[tool] screenshot")
		return resultOrErr(screenshotTool(str("path")))
	default:
		return "未知操作: " + name
	}
}

func numArg(args map[string]interface{}, k string) float64 {
	if v, ok := args[k]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case int:
			return float64(x)
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// pushMessage mengirim pesan tak-diminta dari agent ke chat pengguna (proaktif).
func pushMessage(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	mu.Lock()
	hist = append(hist, Msg{Role: "assistant", Content: text})
	syncActive()
	mu.Unlock()
	broadcast(event{Type: "message", Text: text})
	log.Printf("[push] %s", truncate(text, 200))
	go notify("ZafxyrAgent AI", truncate(text, 80), "http://"+fmt.Sprintf("127.0.0.1:%d", cfg.Port))
}

// scheduleMessage menjadwalkan pesan otomatis setelah delay detik.
func scheduleMessage(delaySec int, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "消息为空，已取消"
	}
	if delaySec < 0 {
		delaySec = 0
	}
	if delaySec > 86400 {
		delaySec = 86400
	}
	go func() {
		time.Sleep(time.Duration(delaySec) * time.Second)
		pushMessage(text)
	}()
	if delaySec == 0 {
		return "pesan dikirim sekarang"
	}
	return fmt.Sprintf("pesan dijadwalkan dalam %d detik", delaySec)
}

// runCalls dipertahankan agar kompatibel dengan pemanggil lama.
func runCalls(calls []gemFn) string {
	var out []string
	for _, c := range calls {
		out = append(out, execTool(c.Name, c.Args))
	}
	return strings.Join(out, "\n")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeFile(path, content string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("路径为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		os.MkdirAll(dir, 0755)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("gagal tulis %s: %v", path, err)
	}
	return fmt.Sprintf("File dibuat: %s (%d byte)", path, len(content)), nil
}

func readFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("路径为空")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("gagal baca %s: %v", path, err)
	}
	s := string(b)
	if len(s) > 4000 {
		s = s[:4000] + "\n…(dipotong)"
	}
	return s, nil
}

func listDir(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/storage/emulated/0"
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("gagal list %s: %v", path, err)
	}
	var b strings.Builder
	for _, e := range entries {
		kind := "-"
		var size int64
		if e.IsDir() {
			kind = "d"
		} else if info, ierr := e.Info(); ierr == nil {
			size = info.Size()
		}
		fmt.Fprintf(&b, "%s %10d %s\n", kind, size, e.Name())
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		s = "(kosong)"
	}
	return s, nil
}

func makeDir(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("路径为空")
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return "", fmt.Errorf("创建目录失败 %s: %v", path, err)
	}
	return "Folder dibuat: " + path, nil
}

func moveFile(from, to string) (string, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return "", fmt.Errorf("源路径或目标路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(to), 0755); err != nil {
		_ = err
	}
	if err := os.Rename(from, to); err == nil {
		return fmt.Sprintf("Dipindah: %s -> %s", from, to), nil
	}
	if out, err := exec.Command("sh", "-c", "mv -f -- "+shellQuote(from)+" "+shellQuote(to)).CombinedOutput(); err != nil {
		return "", fmt.Errorf("gagal pindah: %v %s", err, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("Dipindah: %s -> %s", from, to), nil
}

func deletePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("路径为空")
	}
	if err := os.RemoveAll(path); err != nil {
		return "", fmt.Errorf("gagal hapus %s: %v", path, err)
	}
	return "Dihapus: " + path, nil
}

func downloadURL(url, path string) (string, error) {
	url = strings.TrimSpace(url)
	path = strings.TrimSpace(path)
	if url == "" || path == "" {
		return "", fmt.Errorf("url 或路径为空")
	}
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}
	resp, err := httpClient(120 * time.Second).Get(url)
	if err != nil {
		return "", fmt.Errorf("gagal unduh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gagal unduh: HTTP %d", resp.StatusCode)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		os.MkdirAll(dir, 0755)
	}
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("gagal simpan %s: %v", path, err)
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return "", fmt.Errorf("gagal tulis %s: %v", path, err)
	}
	return fmt.Sprintf("Terunduh: %s (%d byte)", path, n), nil
}

// slashCommand menangani perintah deterministik (/open, /url, /search) tanpa LLM.
func slashCommand(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		if strings.HasPrefix(t, "$ ") {
			if msg, err := runShell(t[2:]); err != nil {
				return err.Error(), true
			} else {
				return msg, true
			}
		}
		return "", false
	}
	fields := strings.SplitN(t, " ", 2)
	cmd := strings.ToLower(fields[0])
	arg := ""
	if len(fields) > 1 {
		arg = strings.TrimSpace(fields[1])
	}
	switch cmd {
	case "/open", "/buka", "/launch", "/app":
		if arg == "" {
			return "Format: /open <nama app>  contoh: /open WhatsApp", true
		}
		if msg, err := openApp(arg); err != nil {
			return err.Error(), true
		} else {
			return msg, true
		}
	case "/url", "/link":
		if arg == "" {
			return "Format: /url <tautan>  contoh: /url example.com", true
		}
		if msg, err := openURL(arg); err != nil {
			return err.Error(), true
		} else {
			return msg, true
		}
	case "/search", "/cari", "/google":
		if arg == "" {
			return "Format: /search <kata kunci>", true
		}
		if msg, err := webSearch(arg); err != nil {
			return err.Error(), true
		} else {
			return msg, true
		}
	case "/sh", "/shell", "/su":
		if arg == "" {
			return "格式：/sh <命令>  例如 /sh df -h", true
		}
		if msg, err := runShell(arg); err != nil {
			return err.Error(), true
		} else {
			return msg, true
		}
	}
	return "", false
}

// quickIntent menangani perintah imperatif singkat secara deterministik
// (buka app / buka tautan) supaya agent SELALU benar-benar bertindak,
// tanpa bergantung pada kecerdasan tool-calling model. Perintah majemuk
// (mengandung "lalu/dan/kemudian") atau yang menyangkut file dibiarkan
// ke jalur LLM agent.
// quickIntent menangani perintah imperatif singkat secara deterministik:
// buka app / buka URL / buka file atau folder. Untuk yang ambigu (maksud lain,
// perintah majemuk, atau nama yang tidak dikenal) dikembalikan ke LLM agent
// agar bisa berpikir pakai semua tool.
func quickIntent(text string) (string, bool) {
	t := strings.TrimSpace(text)
	low := strings.ToLower(t)
	for _, p := range []string{"tolong ", "coba ", "silakan ", "please ", "bisakah kamu ", "bisa kamu ", "can you ", "bantu "} {
		if strings.HasPrefix(low, p) {
			t = strings.TrimSpace(t[len(p):])
			low = strings.ToLower(t)
			break
		}
	}
	prefixes := []struct {
		p      string
		isPath bool
	}{
		{"buka aplikasi ", false}, {"open aplikasi ", false}, {"buka app ", false}, {"open app ", false},
		{"buka folder ", true}, {"open folder ", true}, {"buka direktori ", true}, {"open dir ", true},
		{"buka file ", true}, {"open file ", true}, {"buka berkas ", true},
		{"buka ", false}, {"open ", false}, {"launch ", false}, {"jalankan ", false}, {"start ", false}, {"masuk ke ", false},
	}
	for _, pf := range prefixes {
		if !strings.HasPrefix(low, pf.p) {
			continue
		}
		arg := strings.TrimSpace(t[len(pf.p):])
		arg = strings.Trim(strings.TrimSpace(arg), "\"'")
		if arg == "" {
			return "", false
		}
		la := strings.ToLower(arg)
		// Perintah majemuk -> biarkan LLM yang menangani langkah-langkahnya.
		for _, conj := range []string{" dan ", " lalu ", " terus ", " kemudian ", ", ", " setelah ", " sambil "} {
			if strings.Contains(la, conj) {
				return "", false
			}
		}
		// 1) File/folder bila eksplisit ("buka file X", "buka folder X")
		if pf.isPath {
			if msg, err := openPath(arg); err == nil {
				return msg, true
			}
			return "", false
		}
		// 2) URL / tautan
		if isURL(la) {
			msg, err := openURL(arg)
			if err != nil {
				return err.Error(), true
			}
			return msg, true
		}
		// 3) Aplikasi yang benar-benar terpasang (prioritas untuk nama app)
		if resolvePackage(arg) != "" {
			msg, err := openApp(arg)
			if err != nil {
				return err.Error(), true
			}
			return msg, true
		}
		// 4) Terlihat seperti file/folder (mis. "Download", "catatan.txt", "/storage/...")
		if looksLikePath(la) {
			if msg, err := openPath(arg); err == nil {
				return msg, true
			}
			return "", false
		}
		// 5) Tidak yakin -> serahkan ke LLM agent.
		return "", false
	}
	return "", false
}

// isURL menebak apakah sebuah string adalah tautan.
func isURL(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "www.") {
		return true
	}
	if s == "" || strings.Contains(s, " ") {
		return false
	}
	for _, tld := range []string{".com", ".net", ".org", ".id", ".co", ".io", ".me", ".app", ".dev", ".xyz", ".info", ".site", ".online", ".shop", ".edu", ".gov"} {
		if strings.Contains(s, tld) {
			return true
		}
	}
	return false
}

type oaToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaMessage struct {
	Content   string       `json:"content"`
	ToolCalls []oaToolCall `json:"tool_calls"`
}

func openAIStep(url string, reqBody map[string]interface{}) (oaMessage, int, string, error) {
	jb, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jb))
	if err != nil {
		return oaMessage{}, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := httpClient(120 * time.Second).Do(req)
	if err != nil {
		return oaMessage{}, 0, "", err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return oaMessage{}, resp.StatusCode, string(body), nil
	}
	var out struct {
		Choices []struct {
			Message oaMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return oaMessage{}, resp.StatusCode, string(body), err
	}
	if len(out.Choices) == 0 {
		return oaMessage{}, resp.StatusCode, string(body), nil
	}
	return out.Choices[0].Message, resp.StatusCode, "", nil
}

// openAIStepStream memanggil chat/completions dengan streaming SSE dan
// memanggil onToken untuk setiap potongan teks. tool_calls digabung per-index.
func openAIStepStream(url string, reqBody map[string]interface{}, onToken func(string)) (oaMessage, int, string, error) {
	reqBody["stream"] = true
	jb, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jb))
	if err != nil {
		return oaMessage{}, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := httpClient(180 * time.Second).Do(req)
	if err != nil {
		return oaMessage{}, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return oaMessage{}, resp.StatusCode, string(b), nil
	}
	reader := bufio.NewReader(resp.Body)
	var content strings.Builder
	type acc struct {
		id   string
		name string
		args string
	}
	calls := map[int]*acc{}
	order := []int{}
	for {
		line, rerr := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 {
				d := chunk.Choices[0].Delta
				if d.Content != "" {
					content.WriteString(d.Content)
					if onToken != nil {
						onToken(d.Content)
					}
				}
				for _, tc := range d.ToolCalls {
					a, ok := calls[tc.Index]
					if !ok {
						a = &acc{}
						calls[tc.Index] = a
						order = append(order, tc.Index)
					}
					if tc.ID != "" {
						a.id = tc.ID
					}
					if tc.Function.Name != "" {
						a.name = tc.Function.Name
					}
					a.args += tc.Function.Arguments
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	msg := oaMessage{Content: content.String()}
	for _, idx := range order {
		a := calls[idx]
		tc := oaToolCall{ID: a.id}
		tc.Function.Name = a.name
		tc.Function.Arguments = a.args
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}
	return msg, 200, "", nil
}

func openAIAgent(model string, msgs []Msg) (string, int, string, error) {
	conv := make([]map[string]interface{}, 0, len(msgs))
	for _, m := range msgs {
		conv = append(conv, map[string]interface{}{"role": m.Role, "content": m.Content})
	}
	tools := openAIToolDecls()
	force := looksLikeCommand(lastUserMsg(msgs))
	url := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	for step := 0; step < maxAgentSteps; step++ {
		reqBody := map[string]interface{}{"model": model, "messages": conv, "max_tokens": cfg.MaxTokens}
		forceNow := force && step == 0
		if len(tools) > 0 {
			reqBody["tools"] = tools
			if forceNow {
				reqBody["tool_choice"] = "required"
			} else {
				reqBody["tool_choice"] = "auto"
			}
		}
		var msg oaMessage
		var statusCode int
		var bodyStr string
		var lastErr error
		got := false
		for attempt := 0; attempt < 3; attempt++ {
			m, st, bs, err := openAIStepStream(url, reqBody, func(tok string) {
				broadcast(event{Type: "token", Text: tok})
			})
			if err != nil {
				lastErr = err
				time.Sleep(500 * time.Millisecond)
				continue
			}
			if st < 200 || st >= 300 {
				if forceNow {
					log.Printf("[model %s] tool_choice=required ditolak (%d), ulangi dengan auto", model, st)
					forceNow = false
					if len(tools) > 0 {
						reqBody["tool_choice"] = "auto"
					}
					continue
				}
				return "", st, bs, nil
			}
			statusCode, bodyStr = st, bs
			msg = m
			if len(msg.ToolCalls) > 0 || strings.TrimSpace(msg.Content) != "" {
				got = true
				break
			}
			log.Printf("[model %s] balasan kosong (percobaan %d), ulangi", model, attempt+1)
			time.Sleep(700 * time.Millisecond)
		}
		if lastErr != nil && !got {
			return "", 0, "", lastErr
		}
		if !got {
			return "", statusCode, bodyStr, nil
		}
		if len(msg.ToolCalls) == 0 {
			log.Printf("[model %s] text-only reply: %q", model, msg.Content)
			return msg.Content, statusCode, "", nil
		}
		log.Printf("[model %s] step %d: %d tool_calls", model, step, len(msg.ToolCalls))
		// Langkah agent: catat tool_call, jalankan, kirim hasilnya balik, ulangi.
		tcs := make([]map[string]interface{}, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			tcs = append(tcs, map[string]interface{}{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
		}
		assistant := map[string]interface{}{"role": "assistant", "tool_calls": tcs}
		if strings.TrimSpace(msg.Content) != "" {
			assistant["content"] = msg.Content
		}
		conv = append(conv, assistant)
		for _, tc := range msg.ToolCalls {
			var args map[string]interface{}
			if tc.Function.Arguments != "" {
				json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			conv = append(conv, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": tc.ID,
				"content":      execTool(tc.Function.Name, args),
			})
		}
	}
	return "我已经连续执行了 24 步，达到单次上限。任务可能还没做完 —— " +
		"直接说「继续」我就接着往下做，或者把范围缩小一点（比如一次只处理一个 App）。", 200, "", nil
}

func callOpenAI(msgs []Msg) (string, error) {
	models := []string{cfg.Model}
	for _, fb := range cfg.Fallback {
		if fb != "" && fb != cfg.Model {
			models = append(models, fb)
		}
	}
	var lastErr error
	for i, m := range models {
		text, status, body, err := openAIAgent(m, msgs)
		if err != nil {
			return "", err
		}
		if status >= 200 && status < 300 {
			if strings.TrimSpace(text) != "" {
				return text, nil
			}
			lastErr = fmt.Errorf("empty response: %s", body)
			continue
		}
		lastErr = fmt.Errorf("HTTP %d: %s", status, body)
		if (status == 402 || status == 429 || status == 404) && i < len(models)-1 {
			log.Printf("model %s gagal (%d), fallback ke %s", m, status, models[i+1])
			continue
		}
		break
	}
	return "", lastErr
}

// openAIToolDecls menerjemahkan tool agent ke format OpenAI/OpenRouter (tools[].function).
func openAIToolDecls() []interface{} {
	prop := func(t, desc string) map[string]interface{} {
		m := map[string]interface{}{"type": t}
		if desc != "" {
			m["description"] = desc
		}
		return m
	}
	add := func(name, desc string, props map[string]interface{}, required []string) map[string]interface{} {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": props,
					"required":   required,
				},
			},
		}
	}
	funcs := []interface{}{
		add("open_app",
			"按名称打开手机上的应用（如 微信、网易云音乐），也可以直接给包名（如 com.tencent.mm）。",
			map[string]interface{}{"app": prop("string", "应用名或包名")},
			[]string{"app"}),
		add("open_url",
			"在手机浏览器里打开一个网址。如果没有写协议，自动补 https://。",
			map[string]interface{}{"url": prop("string", "要打开的网址")},
			[]string{"url"}),
		add("web_search",
			"用 Google 搜索，并在浏览器里打开结果页。",
			map[string]interface{}{"query": prop("string", "搜索关键词")},
			[]string{"query"}),
	}
	if shellAllowed() {
		funcs = append(funcs, add("run_shell",
			"以 root 身份在手机上执行 shell 命令并返回输出。"+
				"用于查看系统信息、批量处理文件、修改设置、读取通知库等。"+
				"例如：'df -h'、'ls -l /sdcard/Download'、'dumpsys battery'。",
			map[string]interface{}{"command": prop("string", "要通过 sh -c 执行的命令")},
			[]string{"command"}))
	}
	funcs = append(funcs,
		add("write_file", "写入或新建文件（已存在则覆盖），父目录会自动创建。必须用绝对路径，如 /storage/emulated/0/Download/note.txt。", map[string]interface{}{"path": prop("string", "文件的绝对路径"), "content": prop("string", "文件内容")}, []string{"path", "content"}),
		add("read_file", "读取一个文本文件的内容。使用绝对路径。", map[string]interface{}{"path": prop("string", "文件的绝对路径")}, []string{"path"}),
		add("list_dir", "列出某个目录下的文件和文件夹。", map[string]interface{}{"path": prop("string", "目录路径，如 /storage/emulated/0/Download")}, []string{"path"}),
		add("make_dir", "创建目录（父目录会一并创建）。", map[string]interface{}{"path": prop("string", "目录路径")}, []string{"path"}),
		add("move_file", "移动或重命名文件/文件夹。", map[string]interface{}{"from": prop("string", "源路径"), "to": prop("string", "目标路径")}, []string{"from", "to"}),
		add("delete_path", "删除文件或文件夹。", map[string]interface{}{"path": prop("string", "要删除的路径")}, []string{"path"}),
		add("download_url", "从网络下载文件并保存到手机上的指定路径。", map[string]interface{}{"url": prop("string", "要下载的文件地址"), "path": prop("string", "保存路径")}, []string{"url", "path"}),
		add("open_path", "用系统的默认应用打开一个文件或文件夹。适用于「打开 catatan.txt」「打开 Download 文件夹」「打开这张图片」这类请求。注意：打开应用请用 open_app，不要用这个。", map[string]interface{}{"path": prop("string", "文件或目录的绝对路径")}, []string{"path"}),
		add("schedule_message", "安排一条定时提醒：到点后 agent 会主动把这条消息发给你。当用户说「10 分钟后提醒我吃药」时使用。", map[string]interface{}{"delay_seconds": prop("integer", "多少秒后发送（延迟秒数）"), "message": prop("string", "到点要发送的提醒内容")}, []string{"delay_seconds", "message"}),
		// ---- UI 自动化：让 agent 真正能操作界面，而不只是开关 App ----
		add("screen", "读取当前界面结构，返回带中心坐标的可点节点列表。需要看界面、找按钮、确认上一步操作是否生效时，优先用这个，比截图省大量 token。", map[string]interface{}{}, []string{}),
		add("tap", "点击屏幕坐标。坐标必须来自 screen 返回的 @(x,y)，不要自己估算。duration_ms>=400 视为长按。", map[string]interface{}{"x": prop("integer", "横坐标"), "y": prop("integer", "纵坐标"), "duration_ms": prop("integer", "按住时长毫秒，默认 80；>=400 为长按")}, []string{"x", "y"}),
		add("swipe", "在屏幕上滑动，用于滚动列表、翻页、下拉通知栏。", map[string]interface{}{"x1": prop("integer", "起点横坐标"), "y1": prop("integer", "起点纵坐标"), "x2": prop("integer", "终点横坐标"), "y2": prop("integer", "终点纵坐标"), "duration_ms": prop("integer", "滑动时长毫秒，默认 300")}, []string{"x1", "y1", "x2", "y2"}),
		add("key", "发送按键事件，用于返回、回到主页、回车、删除等。", map[string]interface{}{"key": prop("string", "HOME/BACK/ENTER/POWER/TAB/DEL/MENU/RECENT/VOLUP/VOLDOWN，或数字 keycode")}, []string{"key"}),
		add("text", "向当前聚焦的输入框输入文字。会优先走 ADBKeyboard 以正确支持中文；没装则降级为 input text（中文可能丢失）。", map[string]interface{}{"text": prop("string", "要输入的文字")}, []string{"text"}),
		add("screenshot", "截屏保存为 PNG 文件并返回路径。只在 screen 看不出内容（纯图形界面）时才用。", map[string]interface{}{"path": prop("string", "保存路径，留空则存到 /storage/emulated/0/Download")}, []string{}),
	)
	return funcs
}

func lastUserMsg(msgs []Msg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// looksLikeCommand menebak apakah pesan adalah perintah aksi (bukan sekadar obrolan).
func looksLikeCommand(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return true
	}
	for _, v := range []string{
		"buka", "open", "launch", "jalankan", "run ", "eksekusi", "download", "unduh",
		"install", "pasang", "hapus", "delete", "buat", "bikin", "tulis", "write",
		"baca", "read", "list", "lihat", "tampilkan", "cek", "check", "pindah",
		"move", "rename", "ganti nama", "kirim", "cari", "search", "restart",
		"matikan", "hidupkan", "set ", "ubah", "edit", "copy", "salin", "unzip",
		"extract", "kompres", "zip", "screenshot", "rekam", "hubungi", "telpon",
		"ingatkan", "ingetin", "reminder", "alarm", "jadwalkan", "jadwal",
	} {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// parseSuggestions memisahkan baris "SARAN: ..." dari balasan menjadi chip pilihan.
func parseSuggestions(reply string) (string, []string) {
	lines := strings.Split(reply, "\n")
	kept := make([]string, 0, len(lines))
	var sugg []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		up := strings.ToUpper(t)
		// 模型可能用半角/全角冒号，也可能直接写「建议：」，都认。
		// 漏认的后果是整行留在正文里，同时 chips 又由兜底逻辑补一份，重复显示。
		isSugg := strings.HasPrefix(up, "SARAN") || strings.HasPrefix(up, "SUGGEST") ||
			strings.HasPrefix(t, "建议")
		if isSugg {
			idx := strings.IndexAny(t, ":：")
			if idx < 0 {
				// 形如 "SARAN-xxx" 的写法
				if i := strings.Index(t, "-"); i > 0 {
					idx = i
				} else {
					continue
				}
			}
			rest := strings.TrimPrefix(t[idx+1:], "-")
			for _, part := range strings.Split(rest, "|") {
				p := strings.TrimSpace(strings.Trim(strings.TrimSpace(part), "\"'“”"))
				if p != "" {
					sugg = append(sugg, p)
				}
			}
			continue
		}
		kept = append(kept, ln)
	}
	clean := strings.TrimSpace(strings.Join(kept, "\n"))
	if len(sugg) > 4 {
		sugg = sugg[:4]
	}
	return clean, sugg
}

// defaultSuggestions menyusun chip cadangan berdasarkan aksi yang barusan dilakukan.
func defaultSuggestions(actions []string) []string {
	has := func(n string) bool {
		for _, a := range actions {
			if a == n {
				return true
			}
		}
		return false
	}
	switch {
	case has("download_url"):
		return []string{"打开刚下载的文件", "看看 Download 文件夹", "查看剩余存储"}
	case has("open_app"):
		return []string{"打开微信", "查看剩余存储", "看看 Download 文件夹"}
	case has("open_path"), has("list_dir"):
		return []string{"看看目录里的内容", "打开别的文件", "查看剩余存储"}
	case has("write_file"), has("make_dir"):
		return []string{"读一下那个文件", "看看 Download 文件夹", "查看剩余存储"}
	case has("run_shell"):
		return []string{"再执行一条命令", "查看剩余存储", "看看 Download 文件夹"}
	case has("web_search"):
		return []string{"打开第一个结果", "搜点别的", "查看剩余存储"}
	case has("open_url"):
		return []string{"搜点别的", "打开哔哩哔哩", "查看剩余存储"}
	case has("schedule_message"):
		return []string{"再设一个提醒", "打开提醒事项", "查看剩余存储"}
	}
	return []string{"查看剩余存储", "看看 Download 文件夹", "打开微信"}
}

// fallbackAck memberi konfirmasi singkat saat model tidak menulis teks (hanya SARAN).
func fallbackAck(actions []string) string {
	for _, a := range actions {
		switch a {
		case "schedule_message":
			return "好，提醒已设好。"
		case "open_app":
			return "好，已打开。"
		case "open_url":
			return "好，已在浏览器里打开。"
		case "download_url":
			return "好，下载完成。"
		case "write_file":
			return "好，文件已创建。"
		case "delete_path":
			return "好，已删除。"
		case "move_file":
			return "好，已移动。"
		case "run_shell":
			return "好，命令已执行。"
		}
	}
	return "好，已完成。"
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "消息为空"})
		return
	}
	mu.Lock()
	hist = append(hist, Msg{Role: "user", Content: msg})
	syncActive()
	mu.Unlock()

	resetActions()

	var reply string
	var err error
	if r2, ok := slashCommand(msg); ok {
		reply = r2
	} else if r2, ok := quickIntent(msg); ok {
		log.Printf("[quickIntent] %q -> %q", msg, r2)
		reply = r2
	} else {
		status("思考中…")
		mu.Lock()
		msgs := make([]Msg, 0, len(hist)+1)
		msgs = append(msgs, Msg{Role: "system", Content: cfg.System})
		msgs = append(msgs, hist...)
		mu.Unlock()
		reply, err = callLLM(msgs)
	}

	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		mu.Lock()
		hist = hist[:len(hist)-1]
		mu.Unlock()
		broadcast(event{Type: "error", Text: err.Error()})
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	acts := snapshotActions()
	clean, sugg := parseSuggestions(reply)
	if strings.TrimSpace(clean) == "" {
		clean = fallbackAck(acts)
	}
	if len(sugg) == 0 {
		sugg = defaultSuggestions(acts)
	}

	mu.Lock()
	hist = append(hist, Msg{Role: "assistant", Content: clean})
	syncActive()
	mu.Unlock()

	broadcast(event{Type: "reply", Text: clean, Suggestions: sugg})
	json.NewEncoder(w).Encode(map[string]interface{}{"reply": clean, "suggestions": sugg})
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	cur := cfg.Model
	list := append([]string(nil), cfg.Models...)
	mu.Unlock()
	if len(list) == 0 {
		list = []string{cur}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"current": cur, "models": list})
}

func handleSetModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	w.Header().Set("Content-Type", "application/json")
	if !setModel(in.Model) {
		json.NewEncoder(w).Encode(map[string]string{"error": "模型名为空"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"model": in.Model})
}

func handleRandomModel(w http.ResponseWriter, r *http.Request) {
	m := randomModel()
	setModel(m)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"model": m})
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	snapshot := append([]Msg(nil), hist...)
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(snapshot)
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	hist = nil
	syncActive()
	mu.Unlock()
	json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
}

// ==================== 会话 API ====================

type sessionMeta struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Count    int       `json:"count"`
	Active   bool      `json:"active"`
	Archived bool      `json:"archived"`
}

func jsonReply(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		wantArchived := r.URL.Query().Get("archived") == "1"
		out := make([]sessionMeta, 0, len(sessions))
		for _, s := range sessions {
			if s.Archived != wantArchived {
				continue
			}
			out = append(out, sessionMeta{
				ID: s.ID, Title: s.Title, Created: s.Created, Updated: s.Updated,
				Count: len(s.Messages), Active: s.ID == activeID, Archived: s.Archived,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
		jsonReply(w, 200, map[string]interface{}{"sessions": out, "active": activeID})

	case http.MethodPost:
		// 新建会话并立刻切过去
		s := makeSession()
		sessions = append(sessions, s)
		activeID = s.ID
		hist = nil
		saveSessions()
		log.Printf("[session] new %s", s.ID)
		jsonReply(w, 200, map[string]interface{}{"ok": true, "id": s.ID})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		idx := -1
		for i, s := range sessions {
			if s.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			jsonReply(w, 404, map[string]interface{}{"ok": false, "error": "not found"})
			return
		}
		sessions = append(sessions[:idx], sessions[idx+1:]...)
		if activeID == id {
			ensureActive()
		}
		saveSessions()
		log.Printf("[session] deleted %s", id)
		jsonReply(w, 200, map[string]interface{}{"ok": true, "active": activeID})

	default:
		jsonReply(w, 405, map[string]interface{}{"ok": false, "error": "method not allowed"})
	}
}

func handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonReply(w, 405, map[string]interface{}{"ok": false})
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	mu.Lock()
	defer mu.Unlock()
	for _, s := range sessions {
		if s.ID == body.ID {
			activeID = s.ID
			hist = append([]Msg(nil), s.Messages...)
			saveSessions()
			jsonReply(w, 200, map[string]interface{}{"ok": true, "id": s.ID, "messages": hist})
			return
		}
	}
	jsonReply(w, 404, map[string]interface{}{"ok": false, "error": "not found"})
}

func handleSessionArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonReply(w, 405, map[string]interface{}{"ok": false})
		return
	}
	var body struct {
		ID       string `json:"id"`
		Archived bool   `json:"archived"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	mu.Lock()
	defer mu.Unlock()

	var target *Session
	for _, s := range sessions {
		if s.ID == body.ID {
			target = s
			break
		}
	}
	if target == nil {
		jsonReply(w, 404, map[string]interface{}{"ok": false, "error": "not found"})
		return
	}
	target.Archived = body.Archived
	target.Updated = time.Now()
	if body.Archived {
		// 归档的可能就是当前会话，切走并保证有一条可用的
		ensureActive()
	}
	saveSessions()
	log.Printf("[session] archived=%v %s", body.Archived, body.ID)
	jsonReply(w, 200, map[string]interface{}{"ok": true, "active": activeID})
}

// handleArchivedClear 一键删除全部已归档会话。
func handleArchivedClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		jsonReply(w, 405, map[string]interface{}{"ok": false})
		return
	}
	mu.Lock()
	defer mu.Unlock()

	kept := make([]*Session, 0, len(sessions))
	removed := 0
	for _, s := range sessions {
		if s.Archived {
			removed++
			continue
		}
		kept = append(kept, s)
	}
	sessions = kept
	ensureActive()
	saveSessions()
	log.Printf("[session] cleared %d archived", removed)
	jsonReply(w, 200, map[string]interface{}{"ok": true, "removed": removed, "active": activeID})
}

// ==================== 模型列表 ====================

var (
	modelCacheMu sync.Mutex
	modelCache   []string
	modelCacheAt time.Time
)

// fetchRemoteModels 向 provider 拉真实可用的模型列表，缓存 10 分钟。
func fetchRemoteModels(force bool) ([]string, bool, error) {
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()

	if !force && len(modelCache) > 0 && time.Since(modelCacheAt) < 10*time.Minute {
		return append([]string(nil), modelCache...), true, nil
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, false, fmt.Errorf("还没配置 API Key")
	}
	base := strings.TrimSuffix(strings.TrimSpace(cfg.BaseURL), "/")
	var ids []string

	if cfg.Provider == "gemini" {
		u := base + "/models?key=" + url.QueryEscape(key)
		resp, err := httpClient(20 * time.Second).Get(u)
		if err != nil {
			return nil, false, fmt.Errorf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if resp.StatusCode != 200 {
			return nil, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 180))
		}
		var out struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, false, err
		}
		for _, m := range out.Models {
			n := strings.TrimPrefix(m.Name, "models/")
			if strings.Contains(n, "gemini") {
				ids = append(ids, n)
			}
		}
	} else {
		req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := httpClient(20 * time.Second).Do(req)
		if err != nil {
			return nil, false, fmt.Errorf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if resp.StatusCode != 200 {
			return nil, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 180))
		}
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, false, err
		}
		for _, m := range out.Data {
			if strings.TrimSpace(m.ID) != "" {
				ids = append(ids, m.ID)
			}
		}
	}

	if len(ids) == 0 {
		return nil, false, fmt.Errorf("端点没有返回任何模型")
	}
	sort.Strings(ids)
	modelCache = ids
	modelCacheAt = time.Now()
	log.Printf("[models] fetched %d from %s", len(ids), base)
	return append([]string(nil), ids...), false, nil
}

func handleModelsRemote(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	ids, cached, err := fetchRemoteModels(force)
	if err != nil {
		// 拉不到就退回配置里的列表，前端至少有东西可选
		jsonReply(w, 200, map[string]interface{}{
			"ok": false, "error": err.Error(),
			"models": cfg.Models, "current": cfg.Model, "fallback": true,
		})
		return
	}
	jsonReply(w, 200, map[string]interface{}{
		"ok": true, "models": ids, "current": cfg.Model, "cached": cached,
	})
}

// handleConfig 让用户在聊天界面里直接改配置，不用 root 编辑器去动 JSON。
// GET 不返回 api_key 明文，只告诉前端「配过没有」。
func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]interface{}{
			"provider":    cfg.Provider,
			"base_url":    cfg.BaseURL,
			"model":       cfg.Model,
			"max_tokens":  cfg.MaxTokens,
			"port":        cfg.Port,
			"has_key":     strings.TrimSpace(cfg.APIKey) != "",
			"allow_shell": shellAllowed(),
			"config_path": configPath(),
		})
	case http.MethodPost:
		var body struct {
			APIKey    string `json:"api_key"`
			BaseURL   string `json:"base_url"`
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"ok":false,"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		if v := strings.TrimSpace(body.APIKey); v != "" {
			cfg.APIKey = v
		}
		if v := strings.TrimSpace(body.BaseURL); v != "" {
			cfg.BaseURL = v
			// provider 必须和 base_url 匹配，否则会用错协议格式静默失败。
			// 指向 Google 的就是 gemini，其余一律按 OpenAI 兼容处理（DeepSeek / OpenRouter / 本地 llama.cpp 都是）。
			if strings.Contains(v, "generativelanguage.googleapis.com") {
				cfg.Provider = "gemini"
			} else {
				cfg.Provider = "openai"
			}
		}
		if v := strings.TrimSpace(body.Model); v != "" {
			cfg.Model = v
			// 把新模型放到可选列表首位并去重，否则下拉框选不中它
			// （原逻辑只改 cfg.Model，但前端选项来自 cfg.Models）。
			list := []string{v}
			for _, m := range cfg.Models {
				if m != v {
					list = append(list, m)
				}
			}
			cfg.Models = list
		}
		if body.MaxTokens > 0 {
			cfg.MaxTokens = body.MaxTokens
		}
		saveConfig()
		log.Printf("[config] updated via UI (key set=%v model=%q)", cfg.APIKey != "", cfg.Model)
		fmt.Fprint(w, `{"ok":true}`)
	default:
		http.Error(w, `{"ok":false,"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func main() {
	defMod := ""
	if exe, err := os.Executable(); err == nil {
		defMod = filepath.Dir(filepath.Dir(exe))
	}
	flag.StringVar(&modDir, "moddir", defMod, "module directory")
	flag.StringVar(&home, "home", "/data/adb/aiagent", "data directory")
	flag.Parse()
	os.MkdirAll(home, 0700)

	log.Printf("aiagent starting moddir=%s home=%s", modDir, home)
	loadConfig()
	loadSessions()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/chat", handleChat)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/model", handleSetModel)
	mux.HandleFunc("/model/random", handleRandomModel)
	mux.HandleFunc("/history", handleHistory)
	mux.HandleFunc("/reset", handleReset)
	mux.HandleFunc("/config", handleConfig)
	mux.HandleFunc("/sessions", handleSessions)
	mux.HandleFunc("/sessions/switch", handleSessionSwitch)
	mux.HandleFunc("/sessions/archive", handleSessionArchive)
	mux.HandleFunc("/sessions/archived", handleArchivedClear)
	mux.HandleFunc("/models/remote", handleModelsRemote)
	mux.HandleFunc("/events", handleEvents)

	// 调试端点：仅在环境变量 AGENTD_DEBUG=1 时挂载，用于在真机上直接验证工具行为
	// （不用先配好 LLM 就能测 screen/tap 这些）。默认不存在，不会污染生产环境。
	if os.Getenv("AGENTD_DEBUG") == "1" {
		mux.HandleFunc("/debug/tool", func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Query().Get("name")
			if name == "" {
				http.Error(w, "missing name", http.StatusBadRequest)
				return
			}
			args := map[string]interface{}{}
			if raw := r.URL.Query().Get("args"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					http.Error(w, "bad args json: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
			log.Printf("[debug] tool=%s args=%v", name, args)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, execToolInner(name, args))
		})
		log.Printf("debug endpoint enabled: /debug/tool")
	}

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	go func() {
		time.Sleep(2 * time.Second)
		notify("ZafxyrAgent AI", "Tap untuk buka chat", "http://"+addr)
	}()
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
