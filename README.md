# 自建 IP 查询接口（v0）

复刻 `api.ipapi.is` 的响应结构，数据全部来自公开来源，无调用配额。

## 数据来源

| 数据 | 来源 | 更新频率 |
| --- | --- | --- |
| 地理位置 | [ipapi-is/ipapi](https://github.com/ipapi-is/ipapi) 免费 geolocation 库 | 仓库不定期（当前副本为 2026-08-01） |
| IP→ASN | [iptoasn.com](https://iptoasn.com)（源自 RouteViews） | 每小时 |
| `company_name` | 各 RIR 的 RDAP `ip/` 对象（网段级 whois） | 按需查询 + 本地缓存 |
| `asn_org` | 各 RIR 的 RDAP `autnum/` 对象 | 按需查询 + 本地缓存 |

`company_name` 是网段级 whois 的注册组织，**不是** ASN 的名字 —— 这是它和 `asn_org`
经常不同的原因（`1.1.1.1` 的 company 是 APNIC，asn_org 是 Cloudflare）。首次查询某个
网段时走一次 RDAP，结果按 RIR 返回的整个网段缓存到 `data/cache/company.json`，
所以查过 `32.5.140.2` 之后，整个 `32.5.0.0/16` 都不再产生外部请求。

`asn_org` 同理，取自 RDAP 的 `autnum/` 对象，是**注册的法人机构名**（`AT&T Enterprises, LLC`）
而非 BGP 表里的 AS 代号。代号另外放在 `asn_name` 字段里（`ATT-INTERNET4`）。ASN 缓存按
号码为键存在 `data/cache/asn-org.json`；全球活跃 ASN 仅 8 万余个，实际流量集中在少数几百个上，
跑一天基本全命中。

## 快速开始

```bash
make data    # 下载数据源并构建 data/mmdb/{geo,asn}.mmdb（约 1 分钟）
make run     # 启动 API，默认 :8080
```

## 网页前端

浏览器打开 `http://localhost:8080/` 就是查询页面：搜索框、批量查询、JSON 高亮（点字段名看含义）、
位置 / 网络 / 组织三张卡片、字段说明和接口示例。整页是一个 `go:embed` 进二进制的 HTML 文件，
不加载任何外部资源，离线机器上也能用。

同一个路径按 `Accept` 协商，**不影响现有 API 调用**：

| 请求 | 返回 |
| --- | --- |
| `Accept` 里排在前面的是 `text/html`（浏览器） | 页面 |
| `*/*`（curl、fetch、各种 SDK） | JSON |
| 任意客户端加 `?format=json` | JSON |
| 任意客户端加 `?format=html` | 页面 |
| `-ui=false` 启动 | 只有 JSON |

页面里的 `?q=` 可直接分享：`http://localhost:8080/?q=1.1.1.1` 浏览器打开是渲染好的结果，
curl 同一个地址仍然是 JSON。

## 接口

```bash
curl 'http://localhost:8080/?q=32.5.140.2'          # 单个查询
curl 'http://localhost:8080/'                        # 查询调用方 IP
curl -X POST -H 'Content-Type: application/json' \
     -d '{"ips":["8.8.8.8","1.1.1.1"]}' \
     http://localhost:8080/                          # 批量，最多 100 个，返回数组
curl 'http://localhost:8080/healthz'                 # 健康检查
```

## 命令行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-addr` | `:8080` | 监听地址 |
| `-mmdb` | `data/mmdb` | mmdb 目录 |
| `-cache` | `data/cache` | RDAP 缓存目录 |
| `-company` | `true` | 关掉则 `company_name` 用 `asn_name` 兜底、`asn_org` 保持 RouteViews 值，全程无外部请求 |
| `-compat` | `false` | 补齐 `is_datacenter`/`is_tor`/`is_proxy`/`is_vpn`/`is_abuser` 为 `false` |
| `-ui` | `true` | 浏览器请求 `/` 时返回网页前端；关掉则任何客户端都只拿到 JSON |
| `-timeout` | `5s` | 单次 company 查询预算 |

## 与 api.ipapi.is 的差异

- **`company_name`**：16 个测试 IP 上与官方**完全一致**（覆盖 5 个 RIR + IPv6）。
- **`cc`**：约 8 成一致。官方即使在免费档也用的是**商业版** geolocation 库；本项目用的是
  仓库里的免费版，国家级准确，城市级不可依赖（响应里的 `accuracy` 字段 1 最准、5 最粗）。
- **`asn_org`**：16 个测试 IP 上 **14/16 一致**。两个差异都不是解析问题：
  `AS3333` 官方返回 whois 的 `as-name`（`RIPE NCC AS`），本项目返回 registrant 机构名
  （`Reseaux IP Europeens Network Coordination Centre (RIPE NCC)`）—— 官方对 `AS7018`
  又用的是 registrant，其自身并不一致；`114.114.114.114` 是**归属 AS 就不同**
  （RouteViews 视角为 AS21859，官方为 AS137702），属 BGP 数据源差异。
- **未实现**：`is_datacenter` / `is_vpn` / `is_proxy` / `is_abuser` / `is_tor`。前四个依赖
  ipapi.is 的商业库，无可靠免费替代；`is_tor` 可接 Tor 官方 exit-list 补上。
  默认这些字段**不出现在响应里**，避免"查不到"被误读成"确定不是"。

## 字段来源标记

响应里带两个 `*_source` 字段，说明该值怎么来的：

| 字段 | 值 | 含义 |
| --- | --- | --- |
| `company_source` | `whois_rdap` | 网段级 whois，与 ipapi.is 一致 |
| | `asn_org` | RDAP 未命中，用 ASN 名兜底 |
| `asn_org_source` | `whois_rdap` | RDAP `autnum` 的注册机构名 |
| | `routeviews` | RDAP 未命中，退回 BGP 表里的 AS 代号 |

## 数据更新

```bash
make data    # 重新下载 + 重建 mmdb
```

`geo.mmdb` 和 `asn.mmdb` 是原子替换（写 `.tmp` 再 rename）。当前进程持有的是旧文件的 mmap，
需要发 `SIGHUP` 让它换用新文件 —— 这是热重载，不中断请求、不需要重启：

```bash
make data && kill -HUP $(pgrep -f ipapi-server)
```

服务器上装了 systemd 的话，`ipapi-update.timer` 每周自动做这件事。

## 发版

推一个 `v*` tag，GitHub Actions 会自动构建两种架构的静态包、生成校验和并发布 Release：

```bash
git tag v0.1.0 && git push origin v0.1.0
```

产物：`ipapi-linux-amd64.tar.gz`、`ipapi-linux-arm64.tar.gz`、`SHA256SUMS`。

发版前工作流会先跑 gofmt / vet / build，再对打包出来的 amd64 二进制做冒烟测试
（确认静态链接且能正常启动报错），任一步失败就不会发布。
也可以在 Actions 页面手动触发 `release`，对已存在的 tag 重新打包（会覆盖同名资产）。

`ci` 工作流在 push 到 main 和 PR 时运行同一套检查，外加 shellcheck 和 systemd unit 语法校验。

## 服务器部署

在开发机上打包（静态链接，服务器不需要装 Go）：

```bash
make release            # 产出 dist/ipapi-linux-amd64.tar.gz，约 4MB
make release ARCH=arm64 # ARM 服务器
```

传到服务器并安装：

```bash
scp dist/ipapi-linux-amd64.tar.gz user@server:/tmp/
ssh user@server
mkdir -p /tmp/ipapi && tar -xzf /tmp/ipapi-linux-amd64.tar.gz -C /tmp/ipapi
sudo /tmp/ipapi/deploy/install.sh
```

`install.sh` 会建 `ipapi` 系统用户、装到 `/opt/ipapi`、首次下载并构建数据库、注册
systemd 服务和每周更新定时器，最后轮询健康检查最多 30 秒。
服务器需要 `curl`、`unzip`、`gzip`、`ss`。

默认端口是 8080。**如果这个端口已被 nginx 等占用**，安装脚本会直接报错退出并列出占用者
（而不是装完才发现不通）。换端口重装：

```bash
sudo PORT=8090 /tmp/ipapi/deploy/install.sh
```

改完记得把 [deploy/nginx.conf](deploy/nginx.conf) 里的 `proxy_pass` 也指到同一端口。

装完之后：

| 命令 | 作用 |
| --- | --- |
| `systemctl status ipapi` | 运行状态 |
| `journalctl -u ipapi -f` | 实时日志 |
| `systemctl reload ipapi` | 热重载数据库（不中断请求） |
| `systemctl start ipapi-update` | 立即更新数据 |
| `systemctl list-timers ipapi-*` | 下次自动更新时间 |

服务默认只监听 `127.0.0.1:8080`，对外用 nginx 反代，示例配置见
[deploy/nginx.conf](deploy/nginx.conf)（含限流和 `X-Forwarded-For` 透传）。

### 数据更新是热重载

`ipapi-update.timer` 每周一凌晨触发，下载新数据、重建 mmdb，然后给主进程发 `SIGHUP`。
服务打开新文件后原子替换，旧的 mmap 保留 60 秒等在途请求走完再释放。
**全程不重启、不掉请求**，也因此更新任务不需要任何提权。

## 规模

以 5000 次/天计算：mmdb 查询走 mmap，单次约 1µs；RDAP 只在遇到新网段时触发，
稳定后每天大约几十次外部请求。单机毫无压力。
# ipapi
