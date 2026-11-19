# 监控与日志

## 三层分开看

| 层 | 工具 | 回答什么问题 |
|---|---|---|
| 指标 | Prometheus `/metrics` | 出问题了吗？趋势如何？ |
| 日志 | journald / 文件（JSON） | 具体哪次请求、哪个原因？ |
| 告警 | Alertmanager | 现在要不要叫人？ |

指标和日志必须**能互相对上**：告警里看到的 `request_id` 能在日志里查到，
日志里的 `event` 名和指标名一一对应。这是设计时就定的约束。

## 指标端点

两个服务都暴露 `/metrics`（Prometheus 文本格式）：

- `remote-storage-server` → `http://127.0.0.1:8443/metrics`
- `storage-lock-cloud-api` → `http://127.0.0.1:18000/metrics`

**默认只监听回环**，和业务接口同一个端口。这意味着：

- Prometheus 与服务同机可以直连；
- 跨机抓取需要 SSH 隧道或内网专线；
- **不要把 `/metrics` 经 nginx 转发到公网**。它不含租户 ID，但会泄露流量规模与错误率。

用 `--metrics=false` 可以完全关掉。

## 指标清单

### 远程存储服务（namespace `remote_storage`）

| 指标 | 类型 | 用途 |
|---|---|---|
| `http_requests_total{method,route,status_class}` | counter | 请求量与错误率 |
| `http_request_duration_seconds{...}` | histogram | 延迟分位 |
| `http_in_flight_requests` | gauge | 当前并发 |
| `http_response_bytes_total{route}` | counter | 发现异常大的 List 响应 |
| `storage_operations_total{operation,result}` | counter | 底层存储调用量与失败率 |
| `storage_operation_duration_seconds{operation}` | histogram | 存储层延迟 |
| `storage_errors_total{kind}` | counter | **非预期**存储错误（version_miss 不算） |
| `rate_limited_total{scope}` | counter | 限流命中（ip / tenant） |
| `auth_failures_total` | counter | 鉴权失败 |
| `list_truncated_total` | counter | List 被截断（该扩容了） |
| `panics_total` | counter | panic（任何非零都是 bug） |
| `slow_requests_total` | counter | 慢请求 |
| `build_info{version}` | gauge | 恒为 1，版本在 label 里 |

云端锁 API 用同样的指标名，namespace 换成 `storage_lock_cloud`。

### 低基数是硬约束

label 只用固定枚举（方法、归一化路由、状态码类、操作名、结果）。
**绝不用 `lock_id` / `owner_id` / IP / `request_id`**——那会让每个锁/每个 IP
生成一条新时序，轻则 Prometheus 内存暴涨，重则把时序库打挂。

路由归一化示例：

| 实际请求 | label |
|---|---|
| `/v1/locks/order-42` | `/v1/locks/{id}` |
| `/v1/locks/user-123` | `/v1/locks/{id}` |
| `/wp-admin`（扫描器） | `other` |
| `/api/v1/locks` | `/api/v1/locks` |

有测试 `TestRouteLabelIsLowCardinality` 锁住这条规则。

## 部署 Prometheus

1. 把 `prometheus-scrape.yml` 的 `scrape_configs` 合并进 `prometheus.yml`
2. 把 `alerts.yml` 复制到 `/etc/prometheus/alerts.yml`，并在主配置里引用：

   ```yaml
   rule_files:
     - /etc/prometheus/alerts.yml
   ```

3. `promtool check rules /etc/prometheus/alerts.yml` 验证
   （本仓的 11 条规则已用真实的 `rulefmt` + PromQL parser 校验过）
4. `systemctl reload prometheus`

## Grafana

导入 `grafana-dashboard.json`（Dashboards → Import → 粘贴 JSON）。
面板包含：请求速率、5xx 比例、鉴权失败、在飞请求、延迟 p50/p95/p99、
存储操作速率与延迟、限流命中、异常事件、响应字节、服务状态。

数据源名默认 `Prometheus`，换名字时改模板变量即可。

## 日志

两个服务都是 JSON 输出到 stdout（systemd 下进 journald）。

```bash
--log-level debug|info|warn|error   # 默认 info
--log-format json|text              # 默认 json
```

查询示例：

```bash
# 按 request_id 追一次请求
journalctl -u remote-storage-server -o json | jq 'select(.request_id=="abc123")'

# 看鉴权失败来自哪些 IP
journalctl -u remote-storage-server -o json | jq -r 'select(.event=="auth_failure") | .ip' | sort | uniq -c

# 存储错误
journalctl -u remote-storage-server -o json | jq 'select(.event=="storage_error")'
```

**凭据绝不进日志**：访问日志根本不记录请求头、请求体、查询串——
比"先记录再脱敏"可靠。有测试断言日志里搜不到完整 token。

### journald 容量

不限制会写满磁盘（比服务崩溃更隐蔽）。在 `/etc/systemd/journald.conf` 设：

```ini
SystemMaxUse=200M
```

`install.sh` 会检查并提示。

落独立文件时用 `deploy/logrotate.conf`。

## 告警规则

| 告警 | 条件 | 严重度 |
|---|---|---|
| `RemoteStorageDown` / `CloudAPIDown` | `up==0` 持续 2m | critical |
| `StorageHighErrorRate` | 5xx 占比 >5% 持续 5m | critical |
| `PanicDetected` | 10m 内 panic >0 | critical |
| `HighLatency` | p95 >1s 持续 10m | warning |
| `AuthFailureSpike` | 10m 内鉴权失败 >50 | warning |
| `RateLimitedHeavy` | 10m 内限流 >100 | warning |
| `ListTruncated` | 1h 内截断 >0 | warning |
| `HighContention` | update 错误速率 >10/s 持续 15m | info |
| `SlowRequestsIncreasing` | 15m 内慢请求 >20 | warning |

## 排障路径

```
告警触发
  → 看 Grafana 确认是哪类指标异常（5xx？延迟？限流？）
  → 从告警 annotation 拿到时间点
  → journalctl 按 event 名过滤出具体请求（带 request_id）
  → 用户报障时用 X-Request-Id 响应头直接定位
```
