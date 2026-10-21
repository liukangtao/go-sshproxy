// config.go 实现代理服务器的配置管理功能
// 所有运行参数统一从 proxy.conf.json 读取（命令行不再接受配置参数）
package main

import (
	"bytes"         // 用于把配置文件内容交给解码器
	"encoding/json" // 用于解析配置文件
	"fmt"           // 用于格式化输入输出
	"log"           // 用于记录配置加载信息
	"net"           // 用于监听地址格式校验
	"os"            // 用于访问操作系统功能
	"path/filepath" // 用于配置文件路径处理
	"strconv"       // 用于端口字符串转换
	"strings"       // 用于监听地址中端口的识别
)

// configFileName 配置文件文件名
// 查找顺序：可执行文件所在目录 -> 当前工作目录
const configFileName = "proxy.conf.json"

// Config 代理服务器配置结构体
// 字段名通过 json tag 与配置文件中的键名一一对应；注释即该字段的说明。
type Config struct {
	SSHUser    string `json:"sshUser"`   // SSH 用户名
	SSHPasswd  string `json:"sshPasswd"` // SSH 密码（明文，注意文件权限）
	SSHHost    string `json:"sshHost"`   // SSH 主机地址
	SSHPort    int    `json:"sshPort"`   // SSH 端口，默认 22
	ListenAddr string `json:"listen"`    // 代理监听地址，默认 0.0.0.0（无认证，注意网络可信）

	Socks5Port int `json:"socks5"` // SOCKS5 监听端口，0 表示不启用
	HTTPPort   int `json:"http"`   // HTTP 监听端口，0 表示不启用

	WriteLogToFile bool `json:"log"`        // 是否将路由日志写入 route.log
	Foreground     bool `json:"foreground"` // 是否前台运行；false 时以守护进程方式启动
	Insecure       bool `json:"insecure"`   // 是否跳过 SSH 主机密钥校验（仅调试使用）

	MaxConcurrentConnections int `json:"maxConns"`         // 最大并发建连数（仅握手/建连阶段）
	MaxCacheSize             int `json:"cacheSize"`        // 最大路由缓存条目数
	SSHTimeout               int `json:"sshTimeout"`       // SSH 建连/握手超时（秒）
	SSHPoolSize              int `json:"poolSize"`         // SSH 连接池最大空闲连接数
	HandshakeTimeout         int `json:"handshakeTimeout"` // SOCKS5/HTTP 握手与请求头读取超时（秒）
	RouteTimeout             int `json:"routeTimeout"`     // 路由探测总超时（秒）
	DNSCacheTTLSeconds       int `json:"dnsTTL"`           // DNS 缓存有效期（秒）

	EnableUDP      bool `json:"enableUDP"` // 是否启用 SOCKS5 UDP ASSOCIATE 转发
	UDPIdleTimeout int  `json:"udpIdle"`   // UDP 关联空闲超时（秒），0 表示不超时

	DebugAddr string `json:"debugAddr"` // pprof 调试端点监听地址（空=关闭）
}

// defaultConfig 返回配置默认值：配置文件中省略的字段沿用这些值
func defaultConfig() Config {
	return Config{
		SSHPort:                  22,
		ListenAddr:               "0.0.0.0",
		Socks5Port:               6111,
		HTTPPort:                 8880,
		MaxConcurrentConnections: 200,
		MaxCacheSize:             10000,
		SSHTimeout:               30,
		SSHPoolSize:              50,
		HandshakeTimeout:         60,
		RouteTimeout:             10,
		DNSCacheTTLSeconds:       300,
		EnableUDP:                true,
		UDPIdleTimeout:           300,
	}
}

// GlobalConfig 全局配置实例（包初始化时为默认值，loadConfigFile 加载后覆盖）
var GlobalConfig = defaultConfig()

// validate 校验配置取值，避免非法配置在运行期引发 panic 或静默失效
func (c *Config) validate() error {
	if c.MaxConcurrentConnections <= 0 {
		return fmt.Errorf("maxConns 必须大于 0")
	}
	if c.MaxCacheSize <= 0 {
		return fmt.Errorf("cacheSize 必须大于 0")
	}
	if c.SSHPoolSize <= 0 {
		return fmt.Errorf("poolSize 必须大于 0")
	}
	if c.SSHTimeout <= 0 {
		return fmt.Errorf("sshTimeout 必须大于 0")
	}
	if c.HandshakeTimeout <= 0 {
		return fmt.Errorf("handshakeTimeout 必须大于 0")
	}
	if c.RouteTimeout <= 0 {
		return fmt.Errorf("routeTimeout 必须大于 0")
	}
	if c.DNSCacheTTLSeconds <= 0 {
		return fmt.Errorf("dnsTTL 必须大于 0")
	}
	if c.UDPIdleTimeout < 0 {
		return fmt.Errorf("udpIdle 不能为负数")
	}
	if c.Socks5Port < 0 || c.Socks5Port > 65535 {
		return fmt.Errorf("socks5 必须在 0-65535 之间")
	}
	if c.HTTPPort < 0 || c.HTTPPort > 65535 {
		return fmt.Errorf("http 必须在 0-65535 之间")
	}
	if c.Socks5Port == 0 && c.HTTPPort == 0 {
		return fmt.Errorf("必须至少配置 socks5 或 http 之一")
	}
	if c.Socks5Port > 0 && c.Socks5Port == c.HTTPPort {
		return fmt.Errorf("socks5 与 http 不能使用同一端口")
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		return fmt.Errorf("sshPort 必须在 1-65535 之间")
	}
	if err := validateListenHost(c.ListenAddr); err != nil {
		return err
	}
	if c.DebugAddr != "" {
		if err := validateHostPort(c.DebugAddr, "debugAddr"); err != nil {
			return err
		}
	}
	return nil
}

// validateListenHost 校验监听主机地址。
// 允许留空（监听所有接口）、IP 字面量、localhost 与普通主机名，
// 但不允许携带端口：端口有独立的 socks5/http 配置项，
// 误填 "127.0.0.1:8080" 会在 JoinHostPort 后变成非法地址，启动期才报错。
func validateListenHost(addr string) error {
	if addr == "" || addr == "localhost" {
		return nil
	}
	if net.ParseIP(addr) != nil {
		return nil
	}
	if strings.Contains(addr, ":") {
		return fmt.Errorf("listen 应只填写主机地址（不含端口），当前为 %q；端口请分别配置在 socks5/http 字段", addr)
	}
	return nil
}

// validateHostPort 校验 host:port 形式（用于 debugAddr）
func validateHostPort(addr, field string) error {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s 必须为 host:port 形式: %v", field, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s 端口必须在 1-65535 之间", field)
	}
	return nil
}

// loadConfigFile 从 proxy.conf.json 加载全部配置
// 文件不存在或字段非法都会返回错误：命令行已不再提供配置入口，配置文件是唯一来源。
func loadConfigFile() error {
	path := findConfigFile(configFileName)
	if path == "" {
		return fmt.Errorf("未找到配置文件 %s（应与可执行文件同目录，或放在当前工作目录）", configFileName)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	// 先填充默认值再解码：文件中省略的字段保持默认值
	cfg := defaultConfig()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // 字段名拼错时立即报错，避免静默使用默认值
	if err := dec.Decode(&cfg); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", path, err)
	}

	GlobalConfig = cfg
	log.Printf("已加载配置文件: %s（listen=%s, socks5=%d, http=%d）",
		path, cfg.ListenAddr, cfg.Socks5Port, cfg.HTTPPort)
	return nil
}

// primaryConfigDir 返回配置类文件的首选目录（可执行文件所在目录）
func primaryConfigDir() string {
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "" {
			return dir
		}
	}
	return "."
}

// findConfigFile 在 可执行文件目录、当前工作目录 中查找指定文件
// 返回第一个存在的文件路径；都不存在时返回空字符串
func findConfigFile(name string) string {
	candidates := []string{filepath.Join(primaryConfigDir(), name)}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, name))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
