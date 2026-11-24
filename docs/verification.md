# 验证记录

验证日期：2026-11-22。

- `go test ./...`、`go vet ./...` 通过。
- Windows 上使用 MinGW 和 `CGO_ENABLED=1`，auth、config、platform、jump、proxy 的 `go test -race` 通过。
- Windows Credential Manager 真实写入、读取、删除临时测试凭据通过；没有保留测试凭据。
- 使用 task.txt 提供的测试账户登录并查询 Singapore 在线城市成功。
- 真实 HTTP 转发、HTTPS CONNECT、SOCKS5 请求成功；出口来自 ur.io。客户端严格请求 Singapore 的城市 ID。
- 在指定 Linux 测试服务器上运行临时跳板，本机经跳板登录、查询城市、代理 HTTP/HTTPS 成功。服务器连接表确认存在跳板至 ur.io 的连接。
- Windows 原生 Wintun 成功创建。默认配置前后比较原有 IPv4 路由，无变化。临时加入单个测试目标的主机路由，原始 UDP DNS 查询通过 TUN / ur.io 返回匹配事务 ID 的响应，随后删除该测试路由。
- Linux 在 `unshare -n` 隔离网络命名空间内运行原生 TUN 测试，验证自动路由安装、内核 UDP → TUN → 内核 UDP 往返及关闭后的路由恢复；默认关闭自动路由时未安装默认路由。
- TUN 创建错误返回退出码 1；缺失密码和真实密码错误的非交互调用返回退出码 1。
- Linux 原生运行 jump、proxy、tunnel 测试二进制通过。跳板测试包含错误 PSK、篡改、重放、半关闭、完整转发和取消清理；代理测试包含 HTTP、SOCKS5、UDP ASSOCIATE 和 CONNECT 预读数据。
- Windows amd64、Linux amd64/arm64、macOS amd64/arm64 构建。macOS 当前仅做交叉编译检查，未在真实 Mac 上运行 TUN/Keychain；CI 已配置三系统运行测试。

测试进程、远程临时跳板、临时主机路由和包含凭据的本地测试配置在验证结束后清理。测试使用的账户密码没有写入示例、源码或本文。

## 固定出口追加要求

- 默认启用上游 Fixed IP profile，所有协议共用一个 quality 窗口；窗口及严格准入上限均为 1，关闭备用窗口和定时轮换。
- 禁用 IPv6 能力补齐引起的主动换节点，保留节点失效检测和故障重选。
- 实测三轮共 12 次 HTTP、HTTPS CONNECT 和 SOCKS5 请求，访问不同回显站点，均返回同一个出口 IPv4。
- 针对上游的 `TestSticky*` 测试覆盖：健康出口不轮换、无备用出口分流、传输故障重连原节点、节点消失后重选，以及平台不可达时保留原节点重试。

## 城市 ID 启动校验修复

- 复现 London 搜索结果包含 `018c8210-ce07-0f65-f306-5338d75314d6`，但默认城市列表不包含该 ID，导致旧版启动误报无节点。
- 显式 `exit.location_id` 改为通过 `/network/find-providers2` 按 ID 严格查询节点；使用与实际固定出口相同的能力选择条件，不再依赖默认列表。
- 回归测试覆盖搜索结果 → ID 校验、ID 大小写规范化、ID 优先于旧名称字段、真实无节点、拒绝地区回退及区分 API 故障。
- 使用现有 London 配置实际启动并通过 HTTPS CONNECT 访问外网成功。用户配置未修改，测试实例已停止。
- 全部项目测试、`go vet ./...` 和 Windows/Linux/macOS 构建通过。

## 连接日志与热点 DNS

- Windows 实机确认 ICS 的公用接口为 `utun88`、专用接口为 Wi-Fi 热点，IPv4 转发已启用。
- 旧版未提供 DNS 服务；查询宿主 TUN 地址 `172.31.255.1` 失败，宿主 UDP 53 端口由 SharedAccess 占用。
- 在热点和 TUN 双接口抓包，观察到手机向公共 DNS 的查询被 NAT 为 TUN 源地址，响应也经 TUN 返回。公共 DNS 地址本身不表示绕过隧道。
- 新增 TUN 虚拟 DNS 地址，默认在 `172.31.255.1/30` 网段使用 `.2`。UDP/TCP 监听位于 gVisor 网络栈，不占宿主端口；上游 HTTPS 也通过 ur.io 网络栈拨号。
- 独立临时 Wintun 接口实测：UDP 查询 example.com、TCP 查询 example.org 成功，同实例 HTTPS CONNECT 成功；未停止用户原有实例，测试实例和临时配置已清理。
- 实测普通 DNS 上游的 TCP 53 在所选节点超时，因此默认使用 `https://1.1.1.1/dns-query`，UDP/TCP 客户端查询均通过该 HTTPS 上游。
- 新增测试覆盖真实双用户态栈 UDP/TCP 往返、双向 IP/传输层校验和转换、UDP 响应截断、TCP 完整响应、取消关闭、上游失败 SERVFAIL、错误事务 ID、HTTPS DNS、TCP DNS 分段/乱序/重传、日志级别和出口切换日志。
- `go test ./...`、`go vet ./...`，以及 tunnel/proxy/config 的 `go test -race` 通过。Windows amd64、Linux amd64/arm64、macOS amd64/arm64 构建成功。
- 用户重启新版后，在实际 `utun88` 上再次验证 UDP/TCP DNS 成功。热点抓包确认手机向 `172.31.255.2` 发出查询并收到成功响应；同窗口未观察到物理网卡直接连接 `1.1.1.1:443`，urio 宿主进程连接为 URnetwork 443 通道。

## 随机选择、健康时保持出口

- 首次发现与故障重选请求最多 256 个候选，经过原有严格城市/平台可用性过滤后，用 `crypto/rand` 均匀抽取一个。去重并再次检查故障排除名单，保留 TCP/UDP 能力偏好。
- 关闭多候选并行评价；每次只测试抽中的一个，避免最快节点反复赢得启动竞争。按节点 ID 重连原出口的请求直接透传，不参与随机选择。
- 测试覆盖不同质量排名均能被选中、输入与响应元数据保留、城市回退拒绝、失败节点及中间节点排除、去重、原节点重连、空池和随机源/API 错误。
- 真实 London 发现返回 255 个符合要求的候选，三次独立抽取选中了三个不同节点。
- 两个独立用户态客户端分别获得 `82.108.110.231` 与 `5.101.150.223`，每个客户端连续两次 HTTPS 请求的节点及出口 IP 均保持一致。测试未创建原生 TUN，未修改配置、系统路由或用户运行中的实例。
- 全部项目测试、`go vet ./...`、tunnel 的 `go test -race` 通过；Windows amd64、Linux amd64/arm64、macOS amd64/arm64 构建成功。
- 上游 `TestSticky*` 回归通过，包含保持健康出口、关闭备用/定时轮换、传输中断重连原节点及节点消失后重新发现。
- 可选实网复测：`URIO_TEST_LIVE_RANDOM=1 go test ./internal/tunnel -run '^TestLiveRandomExitSelection$' -v -count=1`，使用当前配置/系统凭据（也可用 `URIO_TEST_CONFIG` 指定配置路径）。
