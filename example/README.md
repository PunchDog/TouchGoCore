# TouchGoCore 示例与运维

## 运行示例网关

1. 用 `-c` / `--config` 指定 **conf 目录**（内含 `config.ini` 与 `gatewayserverbus.json`）。未指定时可用环境变量 `CONFIG_PATH`（指向 conf 或其上级目录）。
2. 启动前会注册 `GatePing`/`GatePong`（协议 `1:1` / `1:2`）。未调用 `util.RegisterProtocolType` 的 `(protocol1, protocol2)` 会被拒绝解析。
3. WebSocket Token：环境变量 `TOUCHGO_WS_TOKEN`。未设置时本地使用 `dev-token`；`TOUCHGO_ENV=production` 时未设置会拒绝认证。登录服示例通过 `?token=` 传递。
4. 监听路径：`ws.url` / `ws.inurl`（如 `/ws` 或 `ws://host:8000/ws`），缺省 `/ws`。

```bash
./gatewayserver -c /opt/touchgo/conf
# 等价
./gatewayserver --config /opt/touchgo/conf
```

登录服连网关并发送 Ping：

```bash
export TOUCHGO_WS_TOKEN=dev-token
./loginserver
```

## nohup 与 systemd

框架已忽略 SIGHUP，关闭仍靠 **SIGTERM**（或 SIGINT）。

- `nohup ./gatewayserver &`：进程在终端退出后仍运行，但缺少重启与统一日志。
- `disown`：仅从当前 shell 作业表移除，不替代进程管理器。
- **推荐 systemd**：使用 [deploy/touchgo.service](deploy/touchgo.service)。`KillSignal=SIGTERM`、`Restart=on-failure`、`ExecStart=... -c /opt/touchgo/conf`。

```bash
sudo cp example/deploy/touchgo.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now touchgo
```

## 配置迁移

旧 JSON 缺少 `allow_origins` / `allowed_origins` 时：

- HTTP CORS：`allow_origins` 为空则不允许跨域。
- WebSocket：`check_origin` 为 true 且 `allowed_origins` 为空时，启动校验失败（连接会被全部拒绝）。

## 生产 checklist

- [ ] WebSocket：设置 `TOUCHGO_WS_TOKEN`（或等价鉴权），`check_origin=true`，填写 `allowed_origins`
- [ ] `trusted_proxies`：仅填入真实反向代理，避免伪造 `X-Forwarded-For`
- [ ] 业务启动前 `RegisterProtocolType` / `RegisterProtocolTypes`
- [ ] gRPC：`tls.enable=true`，`skip_for_intranet=false`；`auth.mode` 使用 `allowlist`、`token` 或 `mtls`（不要用 `none` 对公网）
- [ ] HTTP/WS TLS：直连时配置 `web.tls` / `ws.tls`；前置 Nginx 终结 TLS 时可保持 `enable: false`
- [ ] bidi RPC：`Head.request_id` 用于并发响应关联；旧客户端 `request_id=0` 仍走串行兼容
- [ ] 生产设置 `TOUCHGO_WS_TOKEN`，不要依赖 `dev-token`
- [ ] `metrics.token` 保护 `/metrics`，或仅内网暴露

## RPC request_id

`FSMessage.Head.request_id` 由客户端单调递增，服务端回填同一值。`request_id=0` 表示旧对端，客户端将响应投递给任意一个等待中的请求（串行语义）。下游服务需同步升级 proto 后才能安全并发。
