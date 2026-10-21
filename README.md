# Proxy - 智能网络代理服务器

[![Go Report Card](https://goreportcard.com/badge/github.com/liukangtao/go-sshproxy)](https://goreportcard.com/report/github.com/liukangtao/go-sshproxy)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Proxy 是一个用 Go 语言编写的高性能智能网络代理服务器，支持 SOCKS5 和 HTTP/HTTPS 协议。它能够根据目标地址和网络状况智能选择最佳路由方式（SSH 隧道或直连），并通过连接池管理提高处理效率。

## 功能特性

- **多协议支持**：同时支持 SOCKS5 和 HTTP/HTTPS 代理协议
- **WebSocket 透传**：HTTP 代理支持 Upgrade 请求，101 协议切换后转为双向字节转发
- **智能路由选择**：根据目标地址和网络状况智能选择最佳路由方式
- **SSH 隧道支持**：可通过 SSH 隧道转发网络请求，突破网络限制
- **连接池管理**：高效的 SSH 连接池管理，避免频繁创建和销毁连接
- **路由缓存机制**：缓存目标地址的最佳路由方式，提高处理速度
- **并发控制**：SOCKS5 与 HTTP 共享并发上限，SSH 建连另有并发削峰，防止资源耗尽
- **健康检查**：定期检查连接健康状态，确保连接有效性
- **日志记录**：可选的日志记录功能，便于监控和调试

## 技术架构

```
+---------------------+
|   客户端请求处理    |
| SOCKS5/HTTP代理服务 |
+----------+----------+
           |
+----------v----------+
|   智能路由选择器    |
| (SSH隧道 vs 直连)   |
+----------+----------+
           |
+----------v----------+
|     SSH连接池       |
|  (连接复用管理)     |
+----------+----------+
           |
+----------v----------+
|    目标服务器连接    |
+---------------------+
```


## 安装

### 环境要求

- Go 1.24.0 或更高版本
- SSH 服务器（用于 SSH 隧道转发）

### 构建

```bash
# 克隆项目
git clone https://github.com/liukangtao/go-sshproxy.git
cd go-sshproxy

# 构建
go build

# 运行（需先准备好同目录的 proxy.conf.json）
./proxy
```


## 使用方法

### 基本用法

程序的所有配置都来自与可执行文件同目录（或当前工作目录）的 `proxy.conf.json`，不再接受命令行参数：

```bash
# 1. 编辑 proxy.conf.json
# 2. 启动（默认以守护进程方式运行）
./proxy
```

### 配置文件

仓库只提供脱敏模板 `proxy.conf.json.example`，使用时复制为 `proxy.conf.json` 并填入自己的 SSH 信息：

```bash
cp proxy.conf.json.example proxy.conf.json
```

完整配置示例（省略的字段使用默认值）：

```json
{
  "sshHost": "服务器地址",
  "sshUser": "SSH用户名",
  "sshPasswd": "SSH密码",
  "sshPort": 22,
  "listen": "0.0.0.0",
  "socks5": 6111,
  "http": 8880,
  "log": false,
  "foreground": false,
  "insecure": false,
  "maxConns": 200,
  "cacheSize": 10000,
  "sshTimeout": 30,
  "poolSize": 50,
  "handshakeTimeout": 60,
  "routeTimeout": 10,
  "dnsTTL": 300,
  "enableUDP": true,
  "udpIdle": 300,
  "debugAddr": ""
}
```

> 该文件包含密码明文，请勿公开分享；分发给他人使用时请自行评估风险。
> 字段名拼写错误会在启动时直接报错，不会被静默忽略；未出现的字段使用默认值。

### 配置字段

| 字段 | 默认值 | 说明 |
|------|--------|------|
| sshHost | 空 | SSH 主机地址（必填） |
| sshUser | 空 | SSH 用户名（必填） |
| sshPasswd | 空 | SSH 密码（必填，明文存储） |
| sshPort | 22 | SSH 端口 |
| listen | "0.0.0.0" | 代理监听地址（默认对局域网开放，注意代理无认证；仅需本机使用时改为 "127.0.0.1"） |
| socks5 | 6111 | SOCKS5 代理监听端口，0 表示不启用 |
| http | 8880 | HTTP 代理监听端口，0 表示不启用 |
| log | false | 是否将路由日志写入文件 route.log |
| foreground | false | true 前台运行；false 以守护进程方式运行 |
| insecure | false | 跳过 SSH 主机密钥校验（默认使用 TOFU 指纹固定；仅调试时设为 true） |
| maxConns | 200 | 最大并发建连数（握手/建连阶段，SOCKS5 与 HTTP 共用；进入数据转发后即归还名额） |
| cacheSize | 10000 | 最大路由缓存条目数（小于等于 0 会在启动期被拒绝） |
| sshTimeout | 30 | SSH 建连/握手超时时间（秒） |
| poolSize | 50 | SSH 连接池最大空闲连接数 |
| handshakeTimeout | 60 | SOCKS5/HTTP 握手与请求头读取超时（秒） |
| routeTimeout | 10 | 路由探测总超时（秒） |
| dnsTTL | 300 | DNS 缓存有效期（秒） |
| enableUDP | true | 启用 SOCKS5 UDP ASSOCIATE 转发（UDP 走本机直连，会暴露本机出口 IP） |
| udpIdle | 300 | UDP 关联空闲超时（秒），0 表示不超时 |
| debugAddr | 空 | pprof 性能调试端点监听地址（默认关闭，建议仅绑定 127.0.0.1） |

### 使用示例

#### 浏览器代理设置

1. **SOCKS5 代理**：
   - 地址：localhost
   - 端口：6111

2. **HTTP 代理**：
   - 地址：localhost
   - 端口：8880

#### 命令行工具使用

```bash
# 使用 curl 通过 HTTP 代理访问网站
curl --proxy http://localhost:8880 https://www.google.com

# 使用 curl 通过 SOCKS5 代理访问网站
curl --proxy socks5://localhost:6111 https://www.google.com
```


## 配置说明

### 内部常量

以下参数不开放配置，按固定策略运行：

| 参数 | 值 | 说明 |
|------|----|------|
| 路由负缓存窗口 | 30秒 | 探测失败的目标在窗口内快速失败 |
| HTTP 请求头上限 | 64KB | 超过时回送 `431 Request Header Fields Too Large` |
| HTTP keep-alive 空闲超时 | 60秒 | 等待同一条连接上下一个请求 |
| 连接池健康检查间隔 | 30秒 | keepalive 探测所有 SSH 连接 |
| 空闲 SSH 连接清理 | 1小时 | 超过后关闭并从池中移除 |

> 已建立的 TCP 隧道不设空闲超时（避免中断数据库、WebSocket 等长连接）；SSH 通道本身不支持读写 deadline，其存活性由连接池的 keepalive 健康检查保障。

### 运行文件

| 文件 | 说明 |
|------|------|
| proxy.conf.json | 全部运行配置（必需，与可执行文件同目录） |
| proxy.hostkey | 服务器主机密钥指纹（首次连接自动生成，用于防中间人攻击；服务器重建后需删除） |
| proxy.pid | 服务进程 PID（用于查找/停止进程） |
| proxy.log | 守护进程模式下的运行日志（控制台输出重定向） |
| route.log | 路由选择日志（需配置 `"log": true`） |

## 性能优化

### 连接复用
通过 SSH 连接池实现连接复用，避免频繁创建和销毁连接，显著提高处理效率。

### 智能路由
通过并发测试不同路由方式的响应时间，选择最优路径，提升用户体验。

### 缓存机制
使用路由缓存减少重复的路由选择过程，提高处理速度。

### 资源控制
- `maxConns` 限制握手/建连阶段的并发数（进入数据转发后归还名额）
- `poolSize` 控制 SSH 连接池的最大空闲连接数
- UDP 关联数量与 `maxConns` 同量级上限，避免大量 UDP socket 与协程耗尽文件描述符
- `udpIdle` 控制空闲关联回收；TCP 隧道不设空闲超时，避免中断长连接

## 架构设计

### 模块化设计

项目采用模块化设计，各个功能模块职责清晰：

- [main.go]   - 主程序入口
- [config.go] - 配置管理
- [types.go]  - 数据类型定义
- [pool.go]   - SSH 连接池管理
- [route.go]  - 路由选择和缓存管理
- [socks5.go] - SOCKS5 协议处理
- [http.go]   - HTTP/HTTPS 协议处理
- [udp.go]    - SOCKS5 UDP ASSOCIATE 中继
- [proxy.go]  - 代理服务核心功能
- [debug.go]  - 可选的 pprof 调试端点

### 连接池机制

SSH 连接池通过以下机制提高性能：

1. **连接复用**：多个请求可以共享同一个 SSH 连接
2. **健康检查**：定期检查连接有效性
3. **空闲清理**：释放长时间未使用的连接
4. **容量控制**：限制连接池最大大小

### 智能路由选择

路由选择通过以下步骤实现：

1. **缓存优先**：首先检查路由缓存
2. **并发测试**：同时测试 SSH 隧道和直连方式
3. **性能优化**：选择最快响应的连接方式
4. **缓存更新**：更新路由缓存以供后续请求使用

## 监控与日志

### 日志记录
支持将路由结果记录到日志文件中，在 `proxy.conf.json` 中设置：

```json
{
  "log": true
}
```


日志格式示例：
```
[2026-01-01 10:32:27] 目标:example.com:443 方式:SSH 耗时:226.971086ms
[2026-01-01 10:32:23] 目标:example.org:443 方式:Direct 耗时:77.981082ms
```


### 缓存监控
- 定期清理过期缓存（默认 30 分钟）
- 自动处理超限缓存

### 连接监控
- 定期清理空闲连接（默认 1 小时）
- 监控连接池状态

## 安全考虑

### SSH 安全
- 支持密码认证方式，连接超时控制
- 默认启用 TOFU 主机密钥校验：首次连接记录服务器指纹（proxy.hostkey），之后逐次比对，防止中间人攻击
- 指纹文件不可写时会**拒绝连接**（fail-closed），不会静默降级为"不校验"；请确保可执行文件目录可写
- 把配置项 `insecure` 设为 true 可跳过主机密钥校验，仅建议调试时使用

### 访问控制
- 代理监听地址默认绑定 0.0.0.0（对局域网开放），代理本身无认证，仅限可信网络使用
- 仅需本机使用时，请把配置项 `listen` 改为 `"127.0.0.1"`
- 可扩展实现 IP 白名单等访问控制机制

### UDP 安全
- UDP 关联只接受来自 TCP 控制连接对端 IP 的报文，端口在首个报文到达时锁定
- 只转发"本关联确实发送过报文的目标"的回包，防止任意主机借中继端口向客户端注入数据
- UDP 关联数量受 `maxConns` 同量级上限约束，避免 socket/协程被大量创建
- UDP 不走 SSH 隧道，出口为本机真实网络，会暴露本机出口 IP

### 凭据管理
- SSH 密码存放在 proxy.conf.json 中，不编入程序二进制
- 该文件包含明文凭据，请勿公开分享或提交到代码仓库

## 部署与运维

### 部署方式
- 直接部署 Go 编译生成的二进制文件
- 支持守护进程模式运行

### 运维建议
1. 定期检查日志文件（proxy.log / route.log）大小
2. 监控系统资源使用情况
3. 根据实际使用情况调整 proxy.conf.json 中的配置
4. 停止服务：从 proxy.pid 获取 PID 后结束进程（Windows：`taskkill /PID <pid>`）

### 性能调优
1. 根据实际负载调整 `poolSize`（连接池大小）
2. 根据目标数量调整 `cacheSize`（路由缓存条目数）
3. 根据网络环境调整 `sshTimeout`、`handshakeTimeout`、`routeTimeout`

## 贡献

欢迎提交 Issue 和 Pull Request 来改进这个项目。

### 开发环境设置

```bash
# 克隆项目
git clone https://github.com/liukangtao/go-sshproxy.git
cd go-sshproxy

# 运行测试
go test ./...

# 构建
go build
```

**注意**：本项目仅供学习和研究使用，请遵守相关法律法规，不要用于非法用途。
