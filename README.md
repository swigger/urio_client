# urio-client

一个使用 URnetwork 官方 Go 传输层的 Windows / Linux / macOS 命令行客户端。
同一端口提供 HTTP 代理、HTTPS CONNECT、SOCKS5 CONNECT 和 UDP ASSOCIATE，无代理用户名密码认证。
支持城市出口选择、操作系统密码安全存储、原生 TUN 和 PSK 加密跳板。

## 构建

需要 Git 和 Go。`go.mod` 固定 Go 1.27.4，支持 Go 自动下载工具链。
上游 connect 使用自有 gVisor、QUIC 和 SCTP 分支，bootstrap 脚本固定这些源码版本，不会覆盖已有的不同版本。

Windows PowerShell：

```powershell
./scripts/bootstrap.ps1
go test ./...
./scripts/build.ps1
Copy-Item config.example.yaml config.yaml
./bin/urio.exe --config config.yaml
```

Linux / macOS：

```sh
sh scripts/bootstrap.sh
go test ./...
go build -trimpath -o bin/urio ./cmd/urio
cp config.example.yaml config.yaml
./bin/urio --config config.yaml
```

Windows 构建脚本会将旧 EXE 改名为带时间戳的 `.obsolete-*` 文件再放入新版，保留正在运行的实例。相关进程退出后可删除这些旧文件。

## 配置与密码

编辑 `config.yaml` 的 `account.email`。省略 `account.password` 时，程序先查找操作系统凭据，找不到则在终端隐藏输入。只有登录成功的交互密码才会保存。

- Windows：Credential Manager。
- macOS：Keychain。
- Linux：D-Bus Secret Service，例如 GNOME Keyring / KWallet；服务必须运行且已解锁。无桌面的服务器可以显式配置密码，并限制配置文件访问权限。

```sh
urio clear-password --config config.yaml
urio --config config.yaml --non-interactive
```

清除命令只删除当前 API 地址和账户对应的已存密码，不修改 YAML 中显式配置的密码。
配置密码优先于已存密码。明确的密码错误会清除旧凭据并在终端重新提示，最多尝试三次；网络错误不会删除密码。
`--non-interactive` 禁止所有提示；密码缺失、密码错误、凭据库不可用、配置错误等均以非零状态退出。没有终端时也自动禁止提示。
程序不会将密码或登录令牌写入日志；交互密码无法存入系统凭据库时会明确报错，不另存明文。

## 城市选择

```sh
urio cities --query Singapore
urio cities --query Singapore --select
urio cities --query Singapore --json --non-interactive
```

列表显示城市、地区、国家、在线节点数和可直接填写的 `location_id`。`--select` 提供可搜索的终端列表，选择后输出配置片段。只显示当前有节点的城市；节点数会随网络状态变化。

```yaml
exit:
  city: Singapore
  country: SG
```

也可以填写选择器给出的 `exit.location_id`，优先于城市名。同名城市必须用国家或 ID 消除歧义。
启动时验证在线城市；发现节点时要求严格匹配，节点不可用时不会自动换到其他地区。省略整个 `exit` 则由 URnetwork 自动选择出口。

默认采用固定出口策略：一次运行期间，HTTP、SOCKS5（TCP/UDP）和 TUN 共用一个出口节点；只要节点健康就持续使用，不按时间轮换，也不在多个节点之间分流。短暂传输中断时优先重连原节点，节点离线、持续不健康或确认无法传输时才选择下一个，仍遵守配置的城市限制。
首次启动和故障后需要新出口时，从平台返回的可用候选中均匀随机抽取一个，再单独测试其连通性；失败后重新抽取。每轮请求最多 256 个候选，保留城市、故障排除和协议能力要求，优先使用支持 TCP/UDP 的候选。
随机数来自操作系统，不使用固定种子或按启动时间设种子，使不同机器、进程和启动次数各自独立抽取。随机选择不会触发健康节点轮换，也不保证不同实例一定得到不同公网 IP。
固定的是出口节点；节点运营方改变其公网 IP、IPv4/IPv6 地址差异或程序重启都可能使观察到的 IP 改变。故障切换时，已有 TCP 连接可能需要应用重新连接。
优先选择支持 TCP/UDP 的节点；如果当前节点不支持 IPv6，不会仅为升级 IPv6 能力而更换健康出口。

## 代理

默认监听 `127.0.0.1:1080`，可在 `listen` 中修改。

```sh
curl --proxy http://127.0.0.1:1080 https://api.ipify.org
curl --socks5-hostname 127.0.0.1:1080 https://api.ipify.org
```

SOCKS5 支持 TCP 和 UDP，不支持 BIND、UDP 分片、用户名密码认证。UDP 关联跟随 TCP 控制连接释放，闲置流两分钟回收。
HTTP 转发移除逐跳代理头；CONNECT 和 SOCKS5 域名在 ur.io 用户态网络栈内解析，DNS 请求也经 ur.io 出口。
不提供本机直连回退。底层使用 URnetwork H1/WebSocket 传输；应用 UDP 数据仍可通过隧道传输。

## 日志

默认 `log_level: info`，日志写到 stderr，包含时间、级别、来源和目标：HTTP/SOCKS5 连接发起及结果，TUN TCP/UDP 流，DNS 查询与响应，以及出口选择、原节点重连、故障切换和上游失败原因。
普通健康探测统计放在 debug；同一目标的连续丢包警告合并计数，避免重传刷屏。

```powershell
./bin/urio.exe --log-level info
./bin/urio.exe --log-level debug 2> urio.log
```

也可设置 `warn` / `error`。TUN 日志按连接记录，TCP 重置和超过 45 秒仍未观察到握手响应会警告。
明文 UDP/TCP DNS 可记录域名和答案，代理域名解析记录解析进度；应用自行发送的加密 DoH/DoT 只显示连接地址，不能读取其中的域名。

## TUN

参考 mihomo 使用的 `sing-tun`，Windows 内嵌 Wintun，Linux 使用 `/dev/net/tun`，macOS 使用 utun。
需要管理员/root 权限，Linux 也可授予适当的 `CAP_NET_ADMIN`。TUN 设备创建或运行失败会导致非零退出。

```yaml
tun:
  enabled: true
  name: urio            # macOS 改为 utun（自动选空闲编号）或 utunN
  address: 172.31.255.1/30
  mtu: 1280
  auto_route: false
```

默认只创建设备及设备地址，不安装系统默认路由或修改系统 DNS。操作系统会建立设备自身的直连网段路由；需要使用它的程序/管理员自行选择流量。
启用自动路由时必须显式指定物理上联网卡，防止 ur.io 底层连接再次进入 TUN：

```yaml
tun:
  enabled: true
  name: urio
  auto_route: true
  interface: eth0       # Windows 如 Ethernet，macOS 如 en0
```

自动路由当前接管 IPv4，退出时由 sing-tun 清理；系统 IPv6 路由不变。SOCKS/HTTP 用户态网络栈支持双栈。
不会更改系统 DNS；如原 DNS 位于私网，可自行设置可通过出口访问的公共 DNS。
本程序正常退出使用 Ctrl+C / SIGTERM。不要在依赖 TUN 路由时强制终止进程。

### Windows 热点共享与 DNS

将 TUN 作为 Internet 连接共享的公用接口、Wi-Fi 热点作为专用接口。手机网关使用热点地址（Windows 常见为 `192.168.137.1`）。
对于 `tun.address: 172.31.255.1/30`，手机 DNS 设置为 **`172.31.255.2`**。
`.1` 是宿主机网卡地址，不是 urio DNS 服务；Windows ICS 会占用宿主机的 UDP 53 端口。

TUN 启用时默认提供虚拟 DNS，对 UDP/TCP 53 查询使用 `https://1.1.1.1/dns-query`，HTTPS 连接只经同一个 ur.io 用户态网络栈和出口，没有本机 DNS/直连回退。
虚拟 DNS 不绑定 Windows 的 53 端口，也不修改系统 DNS。可用 `tun.dns_enabled: false` 关闭；`tun.dns_address` 必须是 TUN 网段内未分配给宿主机的可用地址。
`tun.dns_upstream` 可设置为以公共 IPv4 字面量为主机的 HTTPS DNS URL，或 `IPv4:port` 形式的普通 DNS；普通 DNS 的 TCP 支持取决于出口节点。

```powershell
Resolve-DnsName example.com -Server 172.31.255.2 -DnsOnly
Resolve-DnsName example.org -Server 172.31.255.2 -DnsOnly -TcpOnly
```

共享服务会把手机源地址转换成 TUN 地址，因此 TUN 日志可能显示 `172.31.255.1`。公共 DNS 地址也可能通过 TUN，是否走隧道应以路由和抓包为准。

## 加密跳板

跳板部署在能连接 ur.io 的中间服务器。本机登录 API、节点发现以及 ur.io 数据通道都经跳板转发，最终出口仍是所选 ur.io 节点。

服务器：

```sh
urio keygen > jump.psk
chmod 600 jump.psk
cp jump-server.example.yaml server.yaml
urio jump-server --config server.yaml
```

将同一个随机 PSK 安全地复制到客户端，在配置中添加：

```yaml
jump:
  address: jump.example.com:7443
  psk_file: jump.psk
```

`psk_file` 相对于配置文件目录解析，也可用绝对路径。支持直接 `psk` 字段，但不可与 `psk_file` 同时使用。
PSK 至少 32 字节，推荐使用 `keygen` 的 256 位随机值。跳板端无需 ur.io 账户，也不接收账户密码明文；ur.io TLS 保留在加密跳板之内。
跳板默认最多 256 个连接，禁止连接私网、回环、链路本地等地址。确需中转私网时设置 `jump_server.allow_private: true`。
跳板不可用时不回退直连。实现和帧格式见 [协议说明](docs/jump-protocol.md)。

## 验证

```sh
go test ./...
go vet ./...
go test -race ./internal/auth ./internal/jump ./internal/proxy
```

race 检查需要启用 CGO 和本机 C 编译器。CI 在 Windows、Linux、macOS 上运行测试与构建。
可选的真实系统验证：`URIO_TEST_KEYRING=1 go test ./internal/auth -run TestOSCredentialStore`。
Linux 原生 TUN 测试必须在隔离网络命名空间内运行，命令见 `internal/tunnel/native_linux_test.go`。

上游参考：[urnetwork/connect](https://github.com/urnetwork/connect)、[urnetwork/sdk](https://github.com/urnetwork/sdk)、[mihomo](https://github.com/MetaCubeX/mihomo)。相关源码与依赖保留各自许可证。
