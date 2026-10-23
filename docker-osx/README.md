# Docker-OSX · macOS Tahoe (latest) — 功能完整构建

基于 [sickcodes/Docker-OSX](https://github.com/sickcodes/Docker-OSX)（GPLv3+，底层为 kholia/OSX-KVM + OpenCore）的增强版 Dockerfile。
默认运行上游支持的最新 macOS：**Tahoe (26)**，使用 macOS 26 支持的 **MacPro7,1** SMBIOS 与 OSX-KVM 当前（2026-01 起）的 Tahoe OpenCore 配置模板。

> 本镜像仅用于隔离实验/安全研究。macOS 在 Docker/QEMU 中运行请遵守 Apple 软件许可条款（仅限自有授权设备的研究用途）。默认 VNC 无密码，请勿直接暴露公网。

## 一、上游项目分析摘要

| 维度 | 说明 |
|---|---|
| 原理 | 容器内以 `arch` 用户跑 QEMU/KVM，OpenCore 引导 macOS；宿主机 `--device /dev/kvm` 透传硬件虚拟化 |
| 显示 | 默认无头 + VNC（`5999`）；可选 WSLg（X11 + PulseAudio）或 Linux X11 |
| 网络 | QEMU user-net，guest SSH(22) → 容器 10022，屏幕共享(5900)，`ADDITIONAL_PORTS` 追加转发 |
| 磁盘 | OpenCore 引导盘(qcow2, snapshot=on) + BaseSystem 安装器 + `mac_hdd_ng.img`（默认 200G） |
| 序列号 | 构建期用 macserial 生成 MacPro7,1 序列号；运行时支持随机/指定硬件标识 |
| 版本 | high-sierra → **tahoe**；Tahoe/Sonoma+ 使用 `Skylake-Client,-hle,-rtm` |

## 二、本构建相对上游的增强

1. **Tahoe 默认且配置对齐**：MacPro7,1 受支持 SMBIOS；OpenCore 模板取自 OSX-KVM 当前 Tahoe 配置，不再使用 2022 年的旧模板。
2. **CPU 对齐 Tahoe**：`Skylake-Client,-hle,-rtm` + 对应 CPUID flags。
3. **RAM 默认 8GB**，仍支持 `max`/`half`。
4. **CI 钩子 `OSX_COMMANDS`**：guest SSH 就绪后自动在 macOS 内执行命令（分号分隔），日志 `/tmp/osx-commands.log`。
5. **HEALTHCHECK**：检测 QEMU 进程存活（首次下载安装器期间由 start-period 覆盖）。
6. **无头 VNC 默认** + WSLg GUI/音频 override；`AUDIO_DRIVER=auto` 在无 PCM 时自动静音而不是启动失败。
7. **NOPICKER 正确实现**：构建期预生成 `Launch-nopicker.sh`，运行时不再修改持久卷；已有系统盘时可跳过 15GB 安装器下载。
8. **SIZE 真正接线**：构建 arg / 运行 env → `make DISK_SIZE`；仅新建系统盘时生效。
9. **脚本与 stock 引导盘在持久卷之外**（`/home/arch/bin`、`/home/arch/bootdisks`），镜像升级即可生效。
10. **默认 Compose 带文件夹共享与 USB**：宿主 `./share` ↔ guest `/Volumes/hostshare`（9p）；`/dev/bus/usb` + `USB_DEVICE` 透传；macOS 内使用 `mount_9p`。
11. OCI 镜像标签、EXPOSE 10022/5900/5999。

## 三、环境要求

- Linux（或 Windows WSL2 + Docker Desktop / docker-ce），BIOS/系统开启硬件虚拟化
- `/dev/kvm` 可用：`ls -l /dev/kvm`（WSL2 需在 `%USERPROFILE%\.wslconfig` 设置 `[wsl2] nestedVirtualization=true` 后 `wsl --shutdown`）
- CPU 支持 AVX2（macOS Ventura+ 需要；Tahoe 建议）
- 内存 ≥ 16GB 宿主机（给 VM 分 8GB+），磁盘预留：镜像 ~5GB + 安装器下载 ~15GB + 系统盘上限 200G（稀疏）
- 默认无头 VNC 不需要 X11；WSLg GUI 需要 WSLg；Linux X11 需要 X 服务器

## 四、构建

```bash
cd /d/docker-osx            # Windows Git Bash; Linux: cd docker-osx
docker build -t docker-osx-tahoe .

# 国内/慢网加速（按速度选 Arch 镜像源）:
docker build --build-arg RANKMIRRORS=true \
             --build-arg SIZE=200G \
             -t docker-osx-tahoe .

# 或 compose 一键构建+运行（默认无头 VNC）:
docker compose up --build
```

> 默认 Arch mirrorlist 使用 Aliyun → TUNA → geo（可在 Dockerfile 中调整）。
> 如果 Docker Hub 拉取 `archlinux:base-devel` 很慢，可先走镜像再打本地标签：
> `docker pull docker.m.daocloud.io/library/archlinux:base-devel && docker tag docker.m.daocloud.io/library/archlinux:base-devel archlinux:base-devel`
> 构建期使用 codeload tarball 下载上游（避免 github.com git clone 不稳定）。如果 codeload 仍慢，可改用宿主网络：
> `docker build --network=host -t docker-osx-tahoe .`
> 或指定 tarball 镜像前缀（可选）：
> `docker build --network=host --build-arg GITHUB_MIRROR=https://ghproxy.net/https://github.com/ -t docker-osx-tahoe .`

### Build Args

| ARG | 默认 | 说明 |
|---|---|---|
| `SIZE` | `200G` | macOS 系统盘大小；会写入运行时 ENV `SIZE`，可用 `-e SIZE=` 覆盖；仅新建系统盘时生效 |
| `PARALLEL_DOWNLOADS` | `30` | pacman 并行下载数 |
| `RANKMIRRORS` | 空 | 非空即启用按速度排序镜像源 |
| `MIRROR_COUNTRY` / `MIRROR_COUNT` | `US` / `10` | rankmirrors 参数 |
| `GITHUB_MIRROR` | 空 | 可选 GitHub tarball 镜像前缀，如 `https://ghproxy.net/https://github.com/`；空则用 codeload 直连 |
| `STOCK_DEVICE_MODEL` | `MacPro7,1` | stock 引导盘 SMBIOS；macOS 26 支持机型 |
| `STOCK_KERNEL_ARGS` | 空 | stock 引导盘的 macOS boot-args（如 `-no_compat_check`） |

> 不再提供 `LINUX=false`：镜像保留 libguestfs 工具链；引导盘构建使用 mtools/sgdisk 直接写 FAT，避免容器内 vfat 挂载问题。

## 五、运行示例

### 1) 无头 + VNC（默认，推荐）

```bash
docker compose up --build -d
# VNC: localhost:5999（无密码）
docker compose logs -f
```

默认 compose 已带：
- 文件夹共享：宿主 `./share` ↔ guest `/Volumes/hostshare`（启动后在 macOS 内 `sudo -S mount_9p hostshare`）
- USB 管道：`/dev/bus/usb` + USB cgroup 规则；设备参数用 `USB_DEVICE` 指定（见 USB 示例）

或直接：

```bash
docker run -itd --name docker-osx \
  --device /dev/kvm \
  -p 50922:10022 -p 5900:5900 -p 5999:5999 \
  -e EXTRA="-display none -vnc 0.0.0.0:99" \
  docker-osx-tahoe
```

### 2) WSL2 + WSLg GUI + 音频

```bash
docker compose -f docker-compose.yml -f docker-compose.wslg.yml up --build
```

等价于挂载 `/mnt/wslg/.X11-unix` 与 `/mnt/wslg/runtime-dir`，设置 `DISPLAY=:0`、`AUDIO_DRIVER=pa`、`PULSE_SERVER=unix:/mnt/wslg/runtime-dir/pulse/native`。该 override 只覆盖显示/音频，不影响 `SHARE_DIR` 文件夹共享与 USB 透传。

### 3) Linux X11 GUI

在 compose 中取消/添加：

```yaml
    volumes:
      - /tmp/.X11-unix:/tmp/.X11-unix
    environment:
      DISPLAY: "${DISPLAY:-:0.0}"
      EXTRA: ""
      AUDIO_DRIVER: "alsa"
    devices:
      - /dev/kvm:/dev/kvm
      - /dev/snd:/dev/snd
```

或使用 `docker run -v /tmp/.X11-unix:/tmp/.X11-unix -e DISPLAY=$DISPLAY`。

### 4) SSH 进 macOS

```bash
ssh user@localhost -p 50922
```

首次安装完成后，在 macOS 的「系统设置 → 通用 → 共享」开启「远程登录」，并创建对应用户；`OSX_USERNAME`/`OSX_PASSWORD` 默认 `user`/`alpine`。

### 5) CI：装完后自动执行命令

```bash
docker run -itd --device /dev/kvm -p 50922:10022 \
  -e EXTRA="-display none -vnc 0.0.0.0:99" -p 5999:5999 \
  -e OSX_COMMANDS="sw_vers; uname -a" \
  docker-osx-tahoe

# 日志:
docker exec <container> cat /tmp/osx-commands.log
```

runner 依赖 guest 已开启 SSH 且存在 `${OSX_USERNAME}` 账号；`OSX_SSH_RETRIES` 默认 120（×30s）。

### 6) USB 透传（iPhone 等）

Compose 默认已映射 `/dev/bus/usb` 并添加 USB cgroup 规则；只需指定设备参数：

```bash
# WSL2: 先 attach 到 WSL，再启动 compose
usbipd list
usbipd attach --wsl --busid 1-2
USB_DEVICE="hostbus=1,hostaddr=5" docker compose up --build
```

`USB_DEVICE` 也可用 vendor/product：

```bash
USB_DEVICE="vendorid=0x05ac,productid=0x12a8" docker compose up
```

或写入 `.env`（`USB_DEVICE=...`）。入口会执行 `chown -R /dev/bus/usb`；USB 设备应在容器启动前 attach。

手动 docker run：

```bash
docker run -itd --device /dev/kvm \
  --device-cgroup-rule='c 189:* rwm' -v /dev/bus/usb:/dev/bus/usb \
  -p 50922:10022 -p 5999:5999 \
  -e USB_DEVICE="hostbus=1,hostaddr=5" \
  -e EXTRA="-display none -vnc 0.0.0.0:99" \
  docker-osx-tahoe
```

### 7) 文件夹共享（9p，macOS 使用 `mount_9p`）

Compose 默认把宿主项目的 `./share` 挂到容器 `/mnt/hostshare`，并设置 `SHARE_DIR=/mnt/hostshare`；Launch 会自动加：

```
-virtfs local,path=/mnt/hostshare,mount_tag=hostshare,security_model=passthrough,id=hostshare
```

启动后 guest 内（macOS Terminal）：

```bash
sudo -S mount_9p hostshare
# 挂载点通常为 /Volumes/hostshare
```

手动 docker run：

```bash
SHARE=/somefolder
docker run -itd --device /dev/kvm \
  -p 50922:10022 -p 5999:5999 \
  -v "$SHARE:/mnt/hostshare" \
  -e SHARE_DIR=/mnt/hostshare \
  -e EXTRA="-display none -vnc 0.0.0.0:99" \
  docker-osx-tahoe
```

可选 `SHARE_TAG`（默认 hostshare）与 `SHARE_SECURITY_MODEL`（默认 passthrough）；`SHARE_DIR` 留空则不创建 9p 设备。

> 注意：macOS 使用 `mount_9p`，不是 Linux 的 `mount -t 9p`；不要再额外加 `-device virtio-9p-pci`（`-virtfs` 已隐含）。`mapped-xattr` 需要 xattr 支持，WSL/DrvFs 建议用 `passthrough`。

### 8) VNC / SPICE

默认已经是 VNC（`-vnc 0.0.0.0:99` → 宿主机 5999）。如需其他 VNC 端口：

```bash
-e EXTRA="-display none -vnc 0.0.0.0:98" -p 5998:5998
```

SPICE 需要 QEMU 的 SPICE UI 支持；若镜像内 QEMU 包含相关模块：

```bash
-e EXTRA="-display none -spice port=3001,addr=0.0.0.0,disable-ticketing=on" -p 3001:3001
```

> VNC/SPICE 均无认证，务必绑定本机或受信网络。

### 9) 使用自己的系统盘（naked 模式）

```bash
docker run -itd --device /dev/kvm \
  -v "$PWD/mac_hdd_ng.img:/image" \
  -e IMAGE_PATH=/image -e IMAGE_FORMAT=qcow2 \
  -e NOPICKER=true -e EXTRA="-display none -vnc 0.0.0.0:99" -p 5999:5999 \
  docker-osx-tahoe
```

`NOPICKER=true` 会跳过 OpenCore 菜单与安装器下载；`IMAGE_PATH` 不存在时容器会明确报错，不再创建空盘。

### 10) 持久化

`docker-compose.yml` 已使用命名卷 `macos-data:/home/arch/OSX-KVM` 保存安装器、系统盘与生成的引导盘；容器重建不丢数据。

## 六、运行时环境变量速查

| 变量 | 默认 | 说明 |
|---|---|---|
| `SHORTNAME` | `tahoe` | high-sierra / mojave / catalina / big-sur / monterey / ventura / sonoma / sequoia / tahoe |
| `BASESYSTEM_IMAGE` | `/home/arch/OSX-KVM/BaseSystem.img` | 安装器 qcow2 路径；存在则跳过下载 |
| `SIZE` | `200G` | 系统盘大小；仅新建 `mac_hdd_ng.img` 时生效 |
| `IMAGE_PATH` / `IMAGE_FORMAT` | mac_hdd_ng.img / qcow2 | 系统盘路径与格式；naked 模式必须已存在 |
| `BASESYSTEM_FORMAT` | qcow2 | 安装器格式；自备 raw 安装器时改为 raw |
| `RAM` | `8` | GB；`max`=全部内存，`half`=一半 |
| `SMP` / `CORES` | `4` / `4` | vCPU 线程数 / 核心数 |
| `CPU` / `CPUID_FLAGS` | `Skylake-Client` / Tahoe flags | Tahoe/Sonoma+ 建议保持默认 |
| `KVM` | `accel=kvm:tcg` | 无 `/dev/kvm` 时退回 TCG（极慢） |
| `DISPLAY` / `WIDTH` / `HEIGHT` | `:0.0` / 1920 / 1080 | GUI 显示；分辨率用于生成引导盘 |
| `NETWORKING` | `vmxnet3` | `vmxnet3`（快）或 `e1000-82545em`（兼容） |
| `MAC_ADDRESS` | `52:54:00:09:49:17` | QEMU 虚拟网卡 MAC；guest 硬件 MAC 由 SMBIOS/ROM 决定 |
| `INTERNAL_SSH_PORT` / `SCREEN_SHARE_PORT` | 10022 / 5900 | 容器内 QEMU hostfwd 端口 |
| `ADDITIONAL_PORTS` | 空 | 追加 hostfwd，如 `hostfwd=tcp::10023-:80,` |
| `AUDIO_DRIVER` | `auto` | `auto` / `alsa` / `pa` / `none`；无 PCM 时 auto=none |
| `EXTRA` | 空 | 追加 QEMU 参数（VNC/SPICE/额外 USB 等） |
| `SHARE_DIR` | 空（compose 为 `/mnt/hostshare`） | 9p 共享目录；为空则不创建 9p 设备 |
| `SHARE_TAG` / `SHARE_SECURITY_MODEL` | `hostshare` / `passthrough` | 9p mount_tag / security_model |
| `USB_DEVICE` | 空 | QEMU `usb-host` 参数，如 `hostbus=1,hostaddr=5` 或 `vendorid=0x05ac,productid=0x12a8`；空则不接 |
| `BOOT_ARGS` | 空 | 追加到 `-cpu` 的 QEMU CPU 参数（不是 macOS boot-args） |
| `BOOTDISK` | 空 | 自定义 OpenCore 引导盘；空则使用 stock 盘 |
| `NOPICKER` | `false` | true=使用 nopicker 引导盘与 Launch-nopicker.sh，并跳过安装器下载 |
| `GENERATE_UNIQUE` | `false` | true=每次启动生成随机序列号/MAC/引导盘（会改变硬件标识） |
| `GENERATE_SPECIFIC` | `false` | true=用 `DEVICE_MODEL/SERIAL/BOARD_SERIAL/UUID/MAC_ADDRESS` 生成 |
| `MASTER_PLIST` / `MASTER_PLIST_NOPICKER` | 镜像内 Tahoe 模板 | 本地 OpenCore 模板路径 |
| `MASTER_PLIST_URL` | 空 | 非空时覆盖本地模板，使用远程 plist |
| `KERNEL_ARGS` | 空 | 写入 OpenCore boot-args，如 `-no_compat_check` |
| `OSX_COMMANDS` | 空 | guest SSH 就绪后执行的分号分隔命令 |
| `OSX_USERNAME` / `OSX_PASSWORD` / `OSX_SSH_RETRIES` | user / alpine / 120 | CI 钩子账号与轮询 |
| `ENV` | `/env` | 兼容上游；GENERATE_SPECIFIC 可 source，GENERATE_UNIQUE 可写入 |

## 七、端口

| 宿主机(默认) | 容器→guest | 用途 |
|---|---|---|
| 50922 | 10022 → guest:22 | SSH 进 macOS |
| 5900 | 5900 → guest:5900 | macOS 屏幕共享（guest 内开启后） |
| 5999 | 5999 → QEMU VNC :99 | 默认无头显示 |
| 自定义 | `ADDITIONAL_PORTS` | 任意转发（Nginx/开发服务等） |

## 八、首次启动流程（Tahoe）

1. 容器启动后自动下载 Tahoe 安装器并转换为 qcow2（约 15GB，视网络 10~30 分钟；HEALTHCHECK 此时为 `starting` 属正常）
2. 用 VNC 客户端连接 `localhost:5999`；OpenCore 菜单选择 `Base System`
3. 磁盘工具抹掉 `Macintosh HD`（APFS）→ 重新安装 macOS
4. 安装完成后创建账号，并在「共享」中开启「远程登录」
5. `ssh user@localhost -p 50922` 进入；命名卷可持久化，之后 `docker start` 复用

## 九、持久卷、升级与尺寸

- `macos-data` 卷保存：`BaseSystem.img`、`mac_hdd_ng.img`、运行时生成的引导盘、OSX-KVM 上游文件。
- `./share` 是宿主 bind mount（不是命名卷）；USB 节点来自宿主 `/dev/bus/usb`，设备需在容器启动前 attach。
- 启动脚本、Tahoe 模板、stock 引导盘在镜像内（`/home/arch/bin`、`/home/arch/bootdisks`），重建镜像后会更新。
- **升级上游 OpenCore/EFI 后**，卷内 OSX-KVM 上游文件仍是旧版本；使用 `GENERATE_UNIQUE`/`GENERATE_SPECIFIC` 时建议重建卷（会删除已安装 macOS，请先备份）。
- **切换 `SHORTNAME`**：已有 `BaseSystem.img` 时不会重新下载；删除该文件（或换新卷）后再启动。
- **修改 `SIZE`**：只对新建的 `mac_hdd_ng.img` 生效；已有系统盘不会自动扩容。
- 镜像约 **5GB**；首次下载安装器约 **15GB**；系统盘稀疏，随使用增长至 `SIZE`。

## 十、常见问题

| 症状 | 处理 |
|---|---|
| `/dev/kvm: no such file or directory` | 开启硬件虚拟化；WSL2 加 `nestedVirtualization=true` 并重启 WSL |
| 构建时 pacman GPG 报错 | 已内置 keyring 修复；仍失败可加 `--build-arg RANKMIRRORS=true` |
| Tahoe 启动卡死/黑屏 | 确认 `CPU=Skylake-Client` 与 `-hle,-rtm`；内存 ≥8GB；确认使用 MacPro7,1 模板 |
| 安装器提示机型不受支持 | 默认已是 MacPro7,1；若自行改回旧机型，可加 `-e KERNEL_ARGS=-no_compat_check`，但不保证 iMessage/更新 |
| VNC 连不上 | 首次启动要先等 15GB 下载完成；`docker compose logs -f` 查看进度；确认 5999 端口未被占用 |
| 无声音 | 默认无头 VNC 不输出音频；WSLg 用 override（`pa`），Linux 桌面用 `alsa` + `/dev/snd` |
| macOS 内 9p 挂载失败 | 使用 `sudo -S mount_9p hostshare`（不是 `mount -t 9p`）；确认 `SHARE_DIR=/mnt/hostshare` 且 `EXTRA` 没有重复 `-virtfs` |
| 共享目录不可写 | 确认宿主 `./share` 权限；`SHARE_SECURITY_MODEL=passthrough`；guest 内重新 `sudo -S mount_9p hostshare` |
| USB 设备不出现/权限拒绝 | 先 `usbipd attach` 或插好设备再启动；设置 `USB_DEVICE`；确认 `lsusb` 与 `/dev/bus/usb`；入口会 chown |
| CI 命令不执行 | guest 未开启 SSH / 用户不存在 / 端口未通；`docker exec <id> cat /tmp/osx-commands.log` |
| 镜像更新后行为没变 | 数据卷内是旧 OSX-KVM 上游文件；启动脚本/stock 盘在镜像内会更新，运行时生成路径需重建卷 |
| 想换旧版本 | `-e SHORTNAME=sonoma` 并删除旧 `BaseSystem.img`；High Sierra/Mojave 需改用对应 SMBIOS/`GENERATE_SPECIFIC` |

> ⚠️ 容器 sshd 默认允许 root+密码登录、QEMU monitor 走 stdio、VNC 无密码；`device_cgroup_rules` 与 `/dev/bus/usb` 会开放 USB 访问，`./share` 会暴露宿主目录。请仅挂载必要目录、限制端口，勿直接暴露公网。macOS 在 Docker/QEMU 中运行请遵守 Apple 软件许可条款。
