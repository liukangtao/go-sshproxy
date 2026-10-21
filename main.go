// main.go 主程序包，实现一个支持SOCKS5和HTTP/HTTPS的智能代理服务器
// 这是整个代理服务器的入口点，负责初始化配置、启动服务和处理系统信号
package main

import (
	"context" // 用于控制goroutine的上下文，实现优雅关闭
	"fmt"     // 用于格式化输入输出
	"log"     // 用于记录系统日志
	"net"     // 用于端口预检
	"os"      // 用于访问操作系统功能，如进程管理
	"os/exec" // 用于执行外部命令，实现守护进程功能
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall" // 用于系统调用信号
	"time"
)

// pidFileName PID 文件名（与可执行文件同目录，便于查找进程）
const pidFileName = "proxy.pid"

// daemonChildEnv 守护模式派生标记：父进程以守护模式启动子进程时写入子进程环境，
// 子进程据此判断"自己已经是守护进程"，不再二次 fork。
// 说明：配置已全部移入 proxy.conf.json，子进程不再通过命令行参数区分运行模式。
const daemonChildEnv = "PROXY_DAEMON_CHILD"

// isDaemonChild 判断当前进程是否由守护模式派生
func isDaemonChild() bool {
	return os.Getenv(daemonChildEnv) == "1"
}

// main 主函数，程序的入口点
// 负责初始化配置、启动代理服务、处理系统信号和优雅关闭
func main() {
	// 配置已全部移入 proxy.conf.json：显式拒绝命令行参数，避免旧用法被静默忽略
	if len(os.Args) > 1 {
		log.Fatalf("命令行参数已废弃，请修改配置文件 %s（字段说明见 README）", configFileName)
	}

	// 加载配置文件：文件是唯一配置来源，缺失或格式错误直接退出
	if err := loadConfigFile(); err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 校验 SSH 连接信息（凭据不再硬编码在程序中，必须写在配置文件里）
	if GlobalConfig.SSHHost == "" || GlobalConfig.SSHUser == "" || GlobalConfig.SSHPasswd == "" {
		log.Fatalf("SSH 连接信息不完整：请在 %s 中填写 sshHost/sshUser/sshPasswd（格式见 README）", configFileName)
	}

	// 校验配置取值：非法值在启动期直接失败，避免运行期 panic 或静默失效
	if err := GlobalConfig.validate(); err != nil {
		log.Fatalf("配置无效: %v", err)
	}

	// 如果配置为前台运行（或本身已是守护模式的派生子进程），则不再启动守护进程
	if !GlobalConfig.Foreground && !isDaemonChild() {
		// 启动前预检端口：端口被占用通常意味着已有实例在运行，
		// 将此错误在入口进程暴露出来（否则子进程会静默失败）
		if err := precheckPorts(); err != nil {
			log.Fatalf("启动前检查失败: %v", err)
		}

		// 守护进程模式下子进程脱离控制台，将日志重定向到文件，避免日志丢失
		logPath := filepath.Join(primaryConfigDir(), "proxy.log")
		// 启动前轮转过大的旧日志（归档为 proxy.log.old），防止无限增长
		rotateIfTooLarge(logPath, maxLogFileSize)
		logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("打开日志文件 %s 失败: %v", logPath, err)
		}

		// 子进程不接收命令行参数：运行模式由配置文件与 daemonChildEnv 共同决定
		cmd := exec.Command(os.Args[0])
		// 先剔除继承来的同名变量再追加：os.Getenv 返回首个匹配项，
		// 若环境中原有 PROXY_DAEMON_CHILD 且值不是 "1"，追加在后不会被读到，
		// 子进程会误判"自己不是守护进程"而再次派生，形成无限 fork
		cmd.Env = append(envWithout(daemonChildEnv), daemonChildEnv+"=1")

		cmd.Stdin = nil
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		setupDaemonSysProcAttr(cmd)

		if err := cmd.Start(); err != nil {
			logFile.Close()
			log.Fatalf("启动守护进程失败: %v", err)
		}

		log.Printf("守护进程已启动，PID: %d", cmd.Process.Pid)
		log.Printf("运行日志: %s", logPath)

		if err := cmd.Process.Release(); err != nil {
			log.Printf("释放子进程失败: %v", err)
		}
		logFile.Close()
		return
	}

	// —— 以下为实际服务进程（监听与数据转发） ——

	// 创建代理服务器实例
	server := NewProxyServer(&GlobalConfig)

	// 启动可选性能调试端点（仅配置项 debugAddr 非空时生效，默认关闭）
	startDebugServer(GlobalConfig.DebugAddr)

	// 创建带取消功能的上下文，用于控制代理服务的生命周期
	tctx, fcancel := context.WithCancel(context.Background())
	defer fcancel()

	// 启动代理服务器：监听器创建失败会同步返回错误，不再在协程里 log.Fatal
	// （后者会跳过 PID 清理、连接池与日志句柄释放）
	if err := server.Start(tctx); err != nil {
		log.Fatalf("启动代理服务失败: %v", err)
	}

	// 监听器就绪后再写 PID 文件，避免启动失败留下过期 PID
	pidPath := filepath.Join(primaryConfigDir(), pidFileName)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0644); err != nil {
		log.Printf("写入 PID 文件失败: %v", err)
		pidPath = ""
	}

	// 创建信号通道，用于接收系统信号
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	// 等待系统信号
	<-signalChan
	log.Println("收到终止信号，正在关闭服务...")

	// 优雅关闭：停止接受新连接、等待在途连接结束后释放资源
	fcancel()
	server.Shutdown(3 * time.Second)

	// 清理 PID 文件
	if pidPath != "" {
		os.Remove(pidPath)
	}

	log.Println("服务已关闭")
}

// envWithout 返回剔除指定键后的环境变量列表。
// 用途：派生守护进程前清理继承来的旧标记，避免同名变量出现两次时
// os.Getenv 取到旧值（Windows 下键名不区分大小写，故用 EqualFold 比较）。
func envWithout(key string) []string {
	prefix := key + "="
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if len(kv) >= len(prefix) && strings.EqualFold(kv[:len(prefix)], prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// precheckPorts 启动前检查监听端口是否可用
// 端口被占用通常意味着已有实例在运行，将错误提前暴露给用户
func precheckPorts() error {
	ports := []int{}
	if GlobalConfig.Socks5Port > 0 {
		ports = append(ports, GlobalConfig.Socks5Port)
	}
	if GlobalConfig.HTTPPort > 0 {
		ports = append(ports, GlobalConfig.HTTPPort)
	}

	for _, port := range ports {
		addr := net.JoinHostPort(GlobalConfig.ListenAddr, strconv.Itoa(port))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("端口 %s 无法监听（可能已有实例在运行）: %v", addr, err)
		}
		l.Close()
	}
	return nil
}
