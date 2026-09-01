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

`geo.mmdb` 和 `asn.mmdb` 是原子替换（写 `.tmp` 再 rename），但当前进程持有的是旧文件的
mmap，重启后才生效。生产环境建议 `make data && systemctl restart ipapi`。

## 规模

以 5000 次/天计算：mmdb 查询走 mmap，单次约 1µs；RDAP 只在遇到新网段时触发，
稳定后每天大约几十次外部请求。单机毫无压力。
# ipapi
