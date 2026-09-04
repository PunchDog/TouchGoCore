# TouchGoCore

Go 游戏服务框架：WebSocket、gRPC、定时器、Lua、Telegram、Gin、Redis/MySQL/Mongo。

## 快速开始

示例网关与登录服见 [example/README.md](example/README.md)。

```bash
export CONFIG_PATH=example
# 需在 example/conf/config.ini 中配置服务器名到 JSON 的映射
go run ./example/gatewayserver
```

## 配置要点

- 启动配置：`CONFIG_PATH` 或可执行文件旁 `conf/config.ini` → 各服 JSON。
- gRPC 使用 `rpc` 字段；历史 `rpc_port` 仍兼容，启动时会归并到 `rpc`。
- WebSocket 路径取自 `ws.url` / `ws.inurl`（可为 `/ws` 或完整 `wss://host/path`），缺省 `/ws`。
- 队列容量与背压：`server.read_buffer` / `write_buffer` / `backpressure`。
- Prometheus：`metrics.enabled`，可选 `metrics.token` 保护 `/metrics`。

## 开发

```bash
go test ./config ./corectx ./db ./rpc ./util ./websocket ./telegram .
```

业务启动前必须 `util.RegisterProtocolType` / `RegisterProtocolTypes`，未注册协议会被拒绝解析。
