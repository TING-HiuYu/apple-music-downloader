package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type wrapperLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type wrapperStatus struct {
	State                  string   `json:"state"`
	Message                string   `json:"message"`
	AuthRequired           bool     `json:"authRequired"`
	RequiresCredentials    bool     `json:"requiresCredentials"`
	RequiresTwoFA          bool     `json:"requiresTwoFA"`
	WrapperAvailable       bool     `json:"wrapperAvailable"`
	WrapperRunning         bool     `json:"wrapperRunning"`
	AccountDatabasePresent bool     `json:"accountDatabasePresent"`
	CodeFilePresent        bool     `json:"codeFilePresent"`
	CodeExpiresInSeconds   int      `json:"codeExpiresInSeconds"`
	RecentEvents           []string `json:"recentEvents,omitempty"`
}

type wrapperManager struct {
	mu                sync.Mutex
	cmd               *exec.Cmd
	state             string
	message           string
	events            []string
	disabled          bool
	credentialProcess bool
	restartRequested  bool
	restartAttempts   int
	loggedOut         bool
}

func newWrapperManager() *wrapperManager {
	disabled := os.Getenv("WRAPPER_DISABLED") == "1"
	manager := &wrapperManager{state: "login-required", message: "请输入 Apple ID 和密码登录 Wrapper。", disabled: disabled}
	if disabled {
		manager.state = "disabled"
		manager.message = "Wrapper 登录已通过 WRAPPER_DISABLED=1 禁用。"
		return manager
	}
	if _, err := os.Stat(wrapperAccountDatabase()); err == nil {
		manager.state = "starting"
		manager.message = "正在使用已有登录会话启动 Wrapper…"
		go func() {
			if err := manager.start(false, "", ""); err != nil {
				manager.setFailure(err.Error())
			}
		}()
	}
	return manager
}

func wrapperAccountDatabase() string {
	return filepath.Join(wrapperFilesDirectory(), "kvs.sqlitedb")
}

func wrapperBinary() string {
	if path := strings.TrimSpace(os.Getenv("AMDL_WRAPPER_BIN")); path != "" {
		return path
	}
	return "/opt/wrapper/wrapper"
}

func (m *wrapperManager) login(username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 320 {
		return errors.New("请输入有效的 Apple ID")
	}
	if password == "" || len(password) > 1024 {
		return errors.New("请输入 Apple ID 密码")
	}
	if m.disabled {
		return errors.New("当前容器已禁用 Wrapper 登录")
	}

	m.mu.Lock()
	if m.runningLocked() {
		m.mu.Unlock()
		return errors.New("Wrapper 登录正在进行，请等待当前步骤完成")
	}
	m.events = nil
	m.loggedOut = false
	m.mu.Unlock()

	// An explicit login replaces a stale or rejected Android Music session
	// inside this container. Wrapper writes kvs.sqlitedb and the related token
	// files directly into this directory; none are copied outside the runtime.
	if err := os.RemoveAll(wrapperFilesDirectory()); err != nil {
		return fmt.Errorf("清理旧 Wrapper 会话失败: %w", err)
	}
	return m.start(true, username, password)
}

func (m *wrapperManager) start(withCredentials bool, username, password string) error {
	m.mu.Lock()
	loggedOut := m.loggedOut
	m.mu.Unlock()
	// An automatic restart may already be queued when the user logs out. Treat
	// it as cancelled; only an explicit login is allowed to clear loggedOut.
	if loggedOut && !withCredentials {
		return nil
	}

	binary := wrapperBinary()
	if _, err := os.Stat(binary); err != nil {
		return errors.New("当前镜像中未找到 Wrapper")
	}
	if err := os.MkdirAll(filepath.Dir(wrapperAccountDatabase()), 0o700); err != nil {
		return fmt.Errorf("创建 Wrapper 运行目录失败: %w", err)
	}

	args := []string{"-H", "0.0.0.0"}
	if withCredentials {
		args = append([]string{"-L", username + ":" + password, "-F"}, args...)
	}
	command := exec.Command(binary, args...)
	command.Dir = "/opt/wrapper"
	command.Env = wrapperEnvironment()
	configureWrapperProcess(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return err
	}

	m.mu.Lock()
	if m.runningLocked() {
		m.mu.Unlock()
		return errors.New("Wrapper 已在运行")
	}
	m.state = "starting"
	m.message = "正在启动 Wrapper…"
	m.cmd = command
	m.credentialProcess = withCredentials
	m.mu.Unlock()

	if err := command.Start(); err != nil {
		m.mu.Lock()
		m.cmd = nil
		m.state = "failed"
		m.message = friendlyWrapperError(err.Error())
		m.mu.Unlock()
		return err
	}
	if withCredentials && len(command.Args) > 2 {
		// Start has already copied argv into the child process. Remove the
		// credential-bearing value from the long-lived Go command object.
		command.Args[2] = "<redacted>"
	}
	username, password = "", ""

	go m.scan(stdout)
	go m.scan(stderr)
	go m.monitorReady(command)
	go func() {
		err := command.Wait()
		m.mu.Lock()
		if m.cmd == command {
			m.cmd = nil
			if m.loggedOut {
				m.restartRequested = false
				m.credentialProcess = false
				m.state = "login-required"
				m.message = "已退出 Wrapper，请重新登录。"
				m.mu.Unlock()
				return
			}
			if m.restartRequested {
				m.restartRequested = false
				m.state = "starting"
				m.message = "登录成功，正在无凭据重启 Wrapper…"
				m.mu.Unlock()
				go func() {
					time.Sleep(750 * time.Millisecond)
					if startErr := m.start(false, "", ""); startErr != nil {
						m.setFailure(startErr.Error())
					}
				}()
				return
			}
			if m.state == "authenticated" {
				m.restartAttempts++
				if m.restartAttempts <= 3 {
					m.state = "starting"
					m.message = "Wrapper 意外退出，正在自动恢复解密服务…"
					m.addEventLocked(m.message)
					log.Printf("Wrapper process exited after authentication: %v", err)
					m.mu.Unlock()
					go func() {
						time.Sleep(time.Second)
						if startErr := m.start(false, "", ""); startErr != nil {
							m.setFailure(startErr.Error())
						}
					}()
					return
				}
				m.state = "failed"
				m.message = "Wrapper 解密服务连续退出，请查看容器日志后重试。"
				m.addEventLocked(m.message)
				log.Printf("Wrapper process repeatedly exited after authentication: %v", err)
			}
			if m.state != "authenticated" {
				wasAwaitingTwoFA := m.state == "awaiting-2fa" || m.state == "verifying-2fa"
				m.state = "failed"
				if wasAwaitingTwoFA {
					m.message = "验证码等待已超时或 Wrapper 已退出。请重新登录；进入双重认证后，请在 60 秒内输入验证码。"
					m.addEventLocked(m.message)
				} else if err != nil && (m.message == "" || m.message == "正在启动 Wrapper…") {
					m.message = friendlyWrapperError(err.Error())
				} else if m.message == "" {
					m.message = "Wrapper 在完成登录前退出，请检查登录信息后重试。"
				}
			}
		}
		m.mu.Unlock()
	}()
	return nil
}

func (m *wrapperManager) monitorReady(command *exec.Cmd) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(wrapperAccountDatabase()); err == nil && wrapperPortReady() {
			var stopCommand *exec.Cmd
			m.mu.Lock()
			if m.cmd == command && m.runningLocked() && !m.loggedOut {
				if m.credentialProcess && !m.restartRequested {
					m.restartRequested = true
					m.credentialProcess = false
					m.state = "starting"
					m.message = "登录成功，正在清除登录凭据并重启 Wrapper…"
					m.addEventLocked(m.message)
					stopCommand = command
				} else if !m.restartRequested {
					m.restartAttempts = 0
					m.state = "authenticated"
					m.message = "Wrapper 已登录，可以开始下载。"
					m.addEventLocked(m.message)
				}
			}
			m.mu.Unlock()
			if stopCommand != nil {
				stopCredentialWrapper(stopCommand)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func wrapperEnvironment() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "USERNAME=") || strings.HasPrefix(item, "PASSWORD=") {
			continue
		}
		environment = append(environment, item)
	}
	return environment
}

func (m *wrapperManager) scan(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		m.handleLine(scanner.Text())
	}
}

func (m *wrapperManager) handleLine(raw string) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "WARNING: linker:") || strings.Contains(line, "ANDROID_ROOT not set") {
		return
	}
	lower := strings.ToLower(line)
	if wrapperDiagnosticLine(lower) {
		log.Printf("Wrapper: %s", line)
	}
	message := ""
	state := ""
	switch {
	case strings.Contains(lower, "mount /dev/urandom failed") || strings.Contains(lower, "mount proc failed"):
		state = "failed"
		message = "Wrapper 缺少容器挂载权限；官方镜像需要以 privileged 模式重新创建容器。"
	case strings.Contains(lower, "logging in"):
		state = "signing-in"
		message = "正在验证 Apple ID，请留意受信任设备上的登录提示。"
	case strings.Contains(line, "2FA: true") || strings.Contains(lower, "enter your 2fa code"):
		state = "awaiting-2fa"
		message = "Apple 要求双重认证。若没有收到通知，请在受信任的 iPhone、iPad 或 Mac 上手动获取六位验证码，并在 60 秒内输入。"
	case strings.Contains(lower, "login failed"):
		state = "failed"
		message = "Apple ID 或密码未通过验证，或者 Apple 拒绝了此次 Android 登录。"
	case strings.Contains(lower, "account is disabled"):
		state = "failed"
		message = "Apple 账号因安全原因被停用，请先在 Apple 账户页面恢复账号。"
	case strings.Contains(lower, "incorrect") && strings.Contains(lower, "verification"):
		state = "awaiting-2fa"
		message = "验证码不正确或已过期，请输入新的六位验证码。"
	case strings.Contains(lower, "starting") || strings.Contains(lower, "initializing ctx"):
		state = "starting"
		message = "Wrapper 正在初始化运行环境…"
	}

	m.mu.Lock()
	if state != "" {
		m.state = state
		m.message = message
		m.addEventLocked(message)
	} else if strings.Contains(line, "dialogHandler:") {
		dialog := friendlyWrapperDialog(line)
		if dialog != "" {
			m.message = dialog
			m.addEventLocked(dialog)
		}
	}
	m.mu.Unlock()
}

func wrapperDiagnosticLine(lower string) bool {
	if strings.Contains(lower, "password") || strings.Contains(lower, "credential") {
		return false
	}
	for _, marker := range []string{
		"response type", "listening", "dialoghandler:", "decrypt", "exception",
		"fatal", "failed", "error", "panic", "crash", "segmentation", "signal", "killed",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func friendlyWrapperDialog(line string) string {
	start := strings.Index(line, "{title: ")
	separator := strings.Index(line, ", message: ")
	if start < 0 || separator < 0 || separator <= start+8 {
		return ""
	}
	title := strings.TrimSpace(line[start+8 : separator])
	message := strings.TrimSuffix(strings.TrimSpace(line[separator+11:]), "}")
	if strings.EqualFold(title, "Info") {
		return ""
	}
	if title == "Sign In" && message == "" {
		return ""
	}
	if message == "" {
		return title
	}
	return title + " " + message
}

func friendlyWrapperError(raw string) string {
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "operation not permitted") {
		return "Wrapper 缺少容器权限；官方镜像需要以 privileged 模式重新创建容器。"
	}
	if strings.Contains(lower, "executable file not found") || strings.Contains(lower, "no such file") {
		return "当前镜像中未找到可用的 Wrapper。"
	}
	return "Wrapper 登录进程已退出，请检查账号状态后重试。"
}

func (m *wrapperManager) addEventLocked(message string) {
	if message == "" || (len(m.events) > 0 && m.events[len(m.events)-1] == message) {
		return
	}
	m.events = append(m.events, message)
	if len(m.events) > 8 {
		m.events = append([]string(nil), m.events[len(m.events)-8:]...)
	}
}

func (m *wrapperManager) setFailure(message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = "failed"
	m.message = friendlyWrapperError(message)
	m.addEventLocked(m.message)
}

func (m *wrapperManager) runningLocked() bool {
	return m.cmd != nil && m.cmd.Process != nil && m.cmd.ProcessState == nil
}

func wrapperPortReady() bool {
	connection, err := net.DialTimeout("tcp", "127.0.0.1:10020", 75*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func (m *wrapperManager) status() wrapperStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := m.runningLocked()
	_, binaryErr := os.Stat(wrapperBinary())
	_, accountErr := os.Stat(wrapperAccountDatabase())
	_, codeErr := os.Stat(filepath.Join(wrapperFilesDirectory(), "2fa.txt"))
	ready := running && accountErr == nil && wrapperPortReady()
	if ready && !m.loggedOut && m.credentialProcess && !m.restartRequested {
		m.restartRequested = true
		m.credentialProcess = false
		m.state = "starting"
		m.message = "登录成功，正在清除登录凭据并重启 Wrapper…"
		m.addEventLocked(m.message)
		command := m.cmd
		go stopCredentialWrapper(command)
	} else if ready && !m.loggedOut && !m.restartRequested {
		m.restartAttempts = 0
		m.state = "authenticated"
		m.message = "Wrapper 已登录，可以开始下载。"
		m.addEventLocked(m.message)
	}
	authRequired := !m.disabled && m.state != "authenticated"
	return wrapperStatus{
		State:                  m.state,
		Message:                m.message,
		AuthRequired:           authRequired,
		RequiresCredentials:    authRequired && !running && m.state != "awaiting-2fa" && m.state != "logging-out",
		RequiresTwoFA:          m.state == "awaiting-2fa" || m.state == "verifying-2fa",
		WrapperAvailable:       binaryErr == nil,
		WrapperRunning:         running,
		AccountDatabasePresent: accountErr == nil,
		CodeFilePresent:        codeErr == nil,
		CodeExpiresInSeconds:   60,
		RecentEvents:           append([]string(nil), m.events...),
	}
}

func (m *wrapperManager) logout() error {
	if m.disabled {
		return errors.New("当前容器已禁用 Wrapper 登录")
	}

	m.mu.Lock()
	m.loggedOut = true
	m.restartRequested = false
	m.credentialProcess = false
	m.state = "logging-out"
	m.message = "正在退出 Wrapper 并清理登录会话…"
	m.addEventLocked(m.message)
	command := m.cmd
	m.mu.Unlock()

	if command != nil {
		stopCredentialWrapper(command)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			m.mu.Lock()
			stopped := m.cmd == nil || !m.runningLocked()
			m.mu.Unlock()
			if stopped {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := os.RemoveAll(wrapperFilesDirectory()); err != nil {
		m.setFailure(fmt.Sprintf("清理 Wrapper 会话失败: %v", err))
		return fmt.Errorf("清理 Wrapper 会话失败: %w", err)
	}

	m.mu.Lock()
	m.state = "login-required"
	m.message = "已退出 Wrapper，请重新登录。"
	m.events = nil
	m.addEventLocked(m.message)
	m.mu.Unlock()
	return nil
}

func (m *wrapperManager) submitTwoFA(code string) error {
	m.mu.Lock()
	running := m.runningLocked()
	m.mu.Unlock()
	if !running {
		return errors.New("Wrapper 登录进程未运行，请重新输入账号密码")
	}
	if err := submitWrapperTwoFA(code); err != nil {
		return err
	}
	m.mu.Lock()
	m.state = "verifying-2fa"
	m.message = "验证码已提交，正在等待 Apple 验证…"
	m.addEventLocked(m.message)
	m.mu.Unlock()
	return nil
}

func (m *wrapperManager) ready() bool {
	status := m.status()
	return status.State == "authenticated" || status.State == "disabled"
}
