#!/usr/bin/docker
# =============================================================================
#  Docker-OSX · macOS Tahoe (latest) — 功能完整构建
# -----------------------------------------------------------------------------
#  基于 : https://github.com/sickcodes/Docker-OSX   (GPLv3+)
#         kholia/OSX-KVM + OpenCore 引导链
#  默认 : SHORTNAME=tahoe，使用 macOS 26 支持的 MacPro7,1 SMBIOS，
#         以及 OSX-KVM 当前的 Tahoe OpenCore 配置模板
#
#  构建:
#    docker build -t docker-osx-tahoe .
#    # 加速镜像源 + 自定义磁盘大小:
#    docker build --build-arg RANKMIRRORS=true \
#                 --build-arg SIZE=200G \
#                 -t docker-osx-tahoe .
#
#  无头 + VNC (默认):
#    docker run -itd --device /dev/kvm -p 50922:10022 -p 5999:5999 \
#      -e EXTRA="-display none -vnc 0.0.0.0:99" \
#      docker-osx-tahoe
#    # VNC: localhost:5999 (无密码，仅限本机/受信网络)
#
#  WSLg GUI + 音频 (Windows WSL2):
#    docker run -itd --device /dev/kvm -p 50922:10022 \
#      -v /mnt/wslg/.X11-unix:/tmp/.X11-unix \
#      -v /mnt/wslg/runtime-dir:/mnt/wslg/runtime-dir \
#      -e DISPLAY=:0 -e AUDIO_DRIVER=pa \
#      -e PULSE_SERVER=unix:/mnt/wslg/runtime-dir/pulse/native \
#      -e XDG_RUNTIME_DIR=/mnt/wslg/runtime-dir \
#      docker-osx-tahoe
#
#  SSH:
#    ssh user@localhost -p 50922        # 首次安装并开启远程登录后可用
# =============================================================================

FROM archlinux:base-devel

LABEL org.opencontainers.image.title="docker-osx-tahoe" \
      org.opencontainers.image.description="macOS Tahoe in Docker via QEMU/KVM + OpenCore." \
      org.opencontainers.image.source="https://github.com/sickcodes/Docker-OSX" \
      org.opencontainers.image.url="https://sick.codes" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      maintainer="based on sickcodes <https://twitter.com/sickcodes>"

SHELL ["/bin/bash", "-c"]

# -----------------------------------------------------------------------------
# 构建参数 (build args)
# -----------------------------------------------------------------------------
ARG SIZE=200G
ARG PARALLEL_DOWNLOADS=30
ARG RANKMIRRORS
ARG MIRROR_COUNTRY=US
ARG MIRROR_COUNT=10
ARG GITHUB_MIRROR=

# -----------------------------------------------------------------------------
# Stage A — 系统准备 (root): keyring / 镜像源 / 基础工具 / arch 用户 / sshd
# -----------------------------------------------------------------------------

RUN perl -i -p -e s/^\#Color/Color$'\n'ParallelDownloads\ =\ ${PARALLEL_DOWNLOADS:=30}/g /etc/pacman.conf

RUN tee /etc/pacman.d/mirrorlist <<< 'Server = https://mirrors.aliyun.com/archlinux/$repo/os/$arch' \
    && tee -a /etc/pacman.d/mirrorlist <<< 'Server = https://mirrors.tuna.tsinghua.edu.cn/archlinux/$repo/os/$arch' \
    && tee -a /etc/pacman.d/mirrorlist <<< 'Server = https://geo.mirror.pkgbuild.com/$repo/os/$arch' \
    && tee -a /etc/pacman.d/mirrorlist <<< 'Server = https://mirror.rackspace.com/archlinux/$repo/os/$arch'

RUN pacman -Sy archlinux-keyring --noconfirm \
    && rm -rf /etc/pacman.d/gnupg \
    && pacman-key --init \
    && pacman-key --populate archlinux

RUN if [[ "${RANKMIRRORS}" ]]; then \
        { pacman -Sy wget --noconfirm || pacman -Syu wget --noconfirm ; } \
        ; wget -O ./rankmirrors "https://raw.githubusercontent.com/sickcodes/Docker-OSX/${BRANCH:=master}/rankmirrors" \
        ; wget -O- "https://www.archlinux.org/mirrorlist/?country=${MIRROR_COUNTRY:-US}&protocol=https&use_mirror_status=on" \
        | sed -e 's/^#Server/Server/' -e '/^#/d' \
        | head -n "$((${MIRROR_COUNT:-10}+1))" \
        | bash ./rankmirrors --verbose --max-time 5 - > /etc/pacman.d/mirrorlist \
        && cat /etc/pacman.d/mirrorlist \
    ; fi

RUN tee -a /etc/pacman.d/gnupg/gpg.conf <<< 'keyserver hkp://keyserver.ubuntu.com' \
    && tee -a /etc/pacman.d/gnupg/gpg.conf <<< 'keyserver hkps://keys.openpgp.org'

# procps-ng 提供 HEALTHCHECK 的 pgrep；sshpass 供 OSX_COMMANDS 使用；
# python 供构建期生成 OpenCore plist 模板。
RUN pacman -Syu --noconfirm \
        git zip vim nano alsa-utils openssh wget curl bc procps-ng sshpass python \
    && ln -sf /bin/vim /bin/vi

RUN useradd arch -p arch \
    && tee -a /etc/sudoers <<< 'arch ALL=(ALL) NOPASSWD: ALL' \
    && mkdir -p /home/arch \
    && chown arch:arch /home/arch

RUN mkdir -p -m 700 /root/.ssh \
    && touch /root/.ssh/authorized_keys \
    && chmod 644 /root/.ssh/authorized_keys

WORKDIR /etc/ssh
RUN tee -a sshd_config <<< 'AllowTcpForwarding yes' \
    && tee -a sshd_config <<< 'PermitTunnel yes' \
    && tee -a sshd_config <<< 'X11Forwarding yes' \
    && tee -a sshd_config <<< 'PasswordAuthentication yes' \
    && tee -a sshd_config <<< 'PermitRootLogin yes' \
    && tee -a sshd_config <<< 'PubkeyAuthentication yes' \
    && tee -a sshd_config <<< 'HostKey /etc/ssh/ssh_host_rsa_key' \
    && tee -a sshd_config <<< 'HostKey /etc/ssh/ssh_host_ecdsa_key' \
    && tee -a sshd_config <<< 'HostKey /etc/ssh/ssh_host_ed25519_key'

# -----------------------------------------------------------------------------
# Stage B — 虚拟化栈 (root): QEMU / libvirt / OVMF / guestfs
# -----------------------------------------------------------------------------

RUN pacman -S --noconfirm \
        bc qemu-desktop libvirt dnsmasq virt-manager openresolv \
        jack2 edk2-ovmf netctl libvirt-dbus wget scrot

# 保留 libguestfs 工具链（guestfs-tools/linux）；引导盘构建使用 mtools/sgdisk。
RUN pacman -S --noconfirm \
        linux linux-headers archlinux-keyring guestfs-tools mkinitcpio pcre pcre2 \
    && libguestfs-test-tool \
    && rm -rf /var/tmp/.guestfs-*

RUN yes | pacman -Scc && rm -rf /var/cache/pacman/pkg/* /var/tmp/.guestfs-*

# -----------------------------------------------------------------------------
# Stage C — 应用层 (arch): 下载上游 tarball、启动脚本、Tahoe 模板、stock 引导盘
# -----------------------------------------------------------------------------

USER arch
WORKDIR /home/arch/OSX-KVM

RUN set -eux; \
    if [[ -n "${GITHUB_MIRROR}" ]]; then \
        BASE="${GITHUB_MIRROR%/}"; \
        OSX_URL="$BASE/kholia/OSX-KVM/archive/refs/heads/master.tar.gz"; \
        OCBIN_URL="$BASE/acidanthera/OcBinaryData/archive/refs/heads/master.tar.gz"; \
        SERGEN_URL="$BASE/sickcodes/osx-serial-generator/archive/refs/heads/master.tar.gz"; \
        OCPKG_URL="$BASE/acidanthera/OpenCorePkg/archive/refs/heads/master.tar.gz"; \
        RAW_URL="$BASE/sickcodes/osx-serial-generator/raw/master/opencore-image-ng.sh"; \
    else \
        OSX_URL="https://codeload.github.com/kholia/OSX-KVM/tar.gz/refs/heads/master"; \
        OCBIN_URL="https://codeload.github.com/acidanthera/OcBinaryData/tar.gz/refs/heads/master"; \
        SERGEN_URL="https://codeload.github.com/sickcodes/osx-serial-generator/tar.gz/refs/heads/master"; \
        OCPKG_URL="https://codeload.github.com/acidanthera/OpenCorePkg/tar.gz/refs/heads/master"; \
        RAW_URL="https://raw.githubusercontent.com/sickcodes/osx-serial-generator/master/opencore-image-ng.sh"; \
    fi; \
    fetch() { curl -fL --retry 5 --retry-delay 5 --retry-all-errors --max-time 900 -o "$2" "$1"; }; \
    fetch "$OSX_URL" /tmp/osx-kvm.tar.gz; \
    fetch "$OCBIN_URL" /tmp/ocbin.tar.gz; \
    fetch "$SERGEN_URL" /tmp/sergen.tar.gz; \
    fetch "$OCPKG_URL" /tmp/ocpkg.tar.gz; \
    fetch "$RAW_URL" /tmp/opencore-image-ng.sh; \
    find /home/arch/OSX-KVM -mindepth 1 -delete; \
    tar -xzf /tmp/osx-kvm.tar.gz --strip-components=1 -C /home/arch/OSX-KVM; \
    mkdir -p /home/arch/OSX-KVM/resources/OcBinaryData /home/arch/OSX-KVM/Docker-OSX/osx-serial-generator /tmp/OpenCorePkg; \
    tar -xzf /tmp/ocbin.tar.gz --strip-components=1 -C /home/arch/OSX-KVM/resources/OcBinaryData; \
    tar -xzf /tmp/sergen.tar.gz --strip-components=1 -C /home/arch/OSX-KVM/Docker-OSX/osx-serial-generator; \
    tar -xzf /tmp/ocpkg.tar.gz --strip-components=1 -C /tmp/OpenCorePkg; \
    make -C /tmp/OpenCorePkg/Utilities/macserial -j4; \
    cp /tmp/OpenCorePkg/Utilities/macserial/macserial /home/arch/OSX-KVM/macserial; \
    cp /tmp/opencore-image-ng.sh /home/arch/OSX-KVM/opencore-image-ng.sh; \
    chmod +x /home/arch/OSX-KVM/macserial /home/arch/OSX-KVM/opencore-image-ng.sh /home/arch/OSX-KVM/Docker-OSX/osx-serial-generator/*.sh; \
    printf '00:0A:27\tApple, Inc.\n' > /home/arch/OSX-KVM/vendor_macs.tsv; \
    rm -rf /tmp/OpenCorePkg /tmp/osx-kvm.tar.gz /tmp/ocbin.tar.gz /tmp/sergen.tar.gz /tmp/ocpkg.tar.gz /tmp/opencore-image-ng.sh

# 上游已从 OVMF_CODE.fd 改名为 OVMF_CODE_4M.fd；此处尽早失败，避免运行时才暴露。
RUN if [[ ! -f /home/arch/OSX-KVM/OVMF_CODE_4M.fd || ! -f /home/arch/OSX-KVM/OVMF_VARS-1920x1080.fd ]]; then \
        printf '%s\n' "OVMF files missing; OSX-KVM may have renamed them again" >&2; \
        exit 1; \
    fi

RUN mkdir -p /home/arch/bin /home/arch/bootdisks /home/arch/OSX-KVM/OpenCore

# 引导盘使用 mtools/sgdisk 直接写 FAT，避免 libguestfs 在容器内挂载 vfat 失败。
RUN sudo pacman -S --noconfirm gptfdisk mtools \
    && yes | sudo pacman -Scc

RUN cat > /home/arch/OSX-KVM/opencore-image-ng.sh <<'OC_EOF' && chmod +x /home/arch/OSX-KVM/opencore-image-ng.sh
#!/bin/bash
set -euo pipefail

CFG=""
IMG=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --cfg) CFG="$2"; shift 2 ;;
        --cfg=*) CFG="${1#*=}"; shift ;;
        --img) IMG="$2"; shift 2 ;;
        --img=*) IMG="${1#*=}"; shift ;;
        *) shift ;;
    esac
done

if [[ -z "${CFG}" || -z "${IMG}" ]]; then
    echo "usage: $0 --cfg <config.plist> --img <OpenCore.qcow2>" >&2
    exit 1
fi
if [[ ! -f "${CFG}" ]]; then
    echo "config not found: ${CFG}" >&2
    exit 1
fi
if [[ ! -d ./EFI ]]; then
    echo "EFI directory not found in $(pwd)" >&2
    exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
RAW="${WORK}/opencore.raw"

# 与上游 opencore-image-ng.sh 相同的 384M GPT 布局：
# p1 EFI: sectors 2048..300000；p2 OpenCore: sectors 302048..784384
truncate -s 384M "${RAW}"
sgdisk --clear \
    --new=1:2048:300000 --typecode=1:EF00 \
    --new=2:302048:784384 --typecode=2:8300 \
    "${RAW}" >/dev/null
sgdisk --attributes=1:set:2 "${RAW}" >/dev/null

P1_SECTORS=$((300000 - 2048 + 1))
P2_SECTORS=$((784384 - 302048 + 1))
truncate -s "$((P1_SECTORS * 512))" "${WORK}/p1.img"
truncate -s "$((P2_SECTORS * 512))" "${WORK}/p2.img"

mformat -i "${WORK}/p1.img" -v EFI ::
mcopy -i "${WORK}/p1.img" -s ./EFI ::
mcopy -i "${WORK}/p1.img" "${CFG}" ::/EFI/OC/config.plist

printf 'fs0:\\EFI\\BOOT\\BOOTx64.efi\n' > "${WORK}/startup.nsh"
mformat -i "${WORK}/p2.img" -v OpenCore ::
mcopy -i "${WORK}/p2.img" "${WORK}/startup.nsh" ::/startup.nsh

dd if="${WORK}/p1.img" of="${RAW}" bs=512 seek=2048 conv=notrunc status=none
dd if="${WORK}/p2.img" of="${RAW}" bs=512 seek=302048 conv=notrunc status=none

rm -f "${IMG}"
qemu-img convert -f raw -O qcow2 "${RAW}" "${IMG}"
OC_EOF

# -----------------------------------------------------------------------------
# 运行时启动脚本（放在持久卷之外，镜像升级即可生效）
# -----------------------------------------------------------------------------

RUN cat > /home/arch/bin/Launch.sh <<'LAUNCH_EOF' && chmod +x /home/arch/bin/Launch.sh
#!/bin/bash
set -eux
DATA_DIR=/home/arch/OSX-KVM
BOOT_DIR=/home/arch/bootdisks

sudo chown $(id -u):$(id -g) /dev/kvm 2>/dev/null || true
sudo chown -R $(id -u):$(id -g) /dev/snd 2>/dev/null || true
sudo chown -R $(id -u):$(id -g) /dev/bus/usb 2>/dev/null || true

[[ "${RAM}" = max ]] && export RAM="$(("$(head -n1 /proc/meminfo | tr -dc "[:digit:]") / 1000000"))"
[[ "${RAM}" = half ]] && export RAM="$(("$(head -n1 /proc/meminfo | tr -dc "[:digit:]") / 2000000"))"

if [[ "${AUDIO_DRIVER:-auto}" == auto ]]; then
    if compgen -G "/dev/snd/pcmC*" >/dev/null; then
        AUDIO_DRIVER=alsa
    else
        AUDIO_DRIVER=none
    fi
fi

SHARE_ARGS=""
if [[ -n "${SHARE_DIR:-}" ]]; then
    SHARE_ARGS="-virtfs local,path=${SHARE_DIR},mount_tag=${SHARE_TAG:-hostshare},security_model=${SHARE_SECURITY_MODEL:-passthrough},id=hostshare"
fi

USB_ARGS=""
if [[ -n "${USB_DEVICE:-}" ]]; then
    USB_ARGS="-device usb-host,bus=xhci.0,${USB_DEVICE}"
fi

BASE_SYSTEM="${BASESYSTEM_IMAGE:-${DATA_DIR}/BaseSystem.img}"
BOOT_DISK="${BOOTDISK:-${BOOT_DIR}/OpenCore.qcow2}"

exec qemu-system-x86_64 -m ${RAM:-4}000 \
-cpu ${CPU:-Skylake-Client},${CPUID_FLAGS:-kvm=on,vendor=GenuineIntel,+invtsc,vmware-cpuid-freq=on,-hle,-rtm,+ssse3,+sse4.2,+popcnt,+avx,+aes,+xsave,+xsaveopt,check}${BOOT_ARGS} \
-machine q35,${KVM-"accel=kvm:tcg"} \
-smp ${CPU_STRING:-${SMP:-4},cores=${CORES:-4}} \
-device qemu-xhci,id=xhci \
-device usb-kbd,bus=xhci.0 -device usb-tablet,bus=xhci.0 \
${USB_ARGS:-} \
-device isa-applesmc,osk=ourhardworkbythesewordsguardedpleasedontsteal\(c\)AppleComputerInc \
-drive if=pflash,format=raw,readonly=on,file=${DATA_DIR}/OVMF_CODE_4M.fd \
-drive if=pflash,format=raw,file=${DATA_DIR}/OVMF_VARS-1920x1080.fd \
-smbios type=2 \
-audiodev ${AUDIO_DRIVER},id=hda -device ich9-intel-hda -device hda-duplex,audiodev=hda \
-device ich9-ahci,id=sata \
-drive id=OpenCoreBoot,if=none,snapshot=on,format=qcow2,file=${BOOT_DISK} \
-device ide-hd,bus=sata.2,drive=OpenCoreBoot \
-device ide-hd,bus=sata.3,drive=InstallMedia \
-drive id=InstallMedia,if=none,file=${BASE_SYSTEM},format=${BASESYSTEM_FORMAT:-qcow2} \
-drive id=MacHDD,if=none,file=${IMAGE_PATH:-${DATA_DIR}/mac_hdd_ng.img},format=${IMAGE_FORMAT:-qcow2} \
-device ide-hd,bus=sata.4,drive=MacHDD \
-netdev user,id=net0,hostfwd=tcp::${INTERNAL_SSH_PORT:-10022}-:22,hostfwd=tcp::${SCREEN_SHARE_PORT:-5900}-:5900,${ADDITIONAL_PORTS} \
-device ${NETWORKING:-vmxnet3},netdev=net0,id=net0,mac=${MAC_ADDRESS:-52:54:00:09:49:17} \
-monitor stdio \
-boot menu=on \
-vga vmware \
${SHARE_ARGS:-} \
${EXTRA:-}
LAUNCH_EOF

# NOPICKER 变体在构建期生成，运行时不再修改持久卷中的脚本。
RUN grep -v InstallMedia /home/arch/bin/Launch.sh > /home/arch/bin/Launch-nopicker.sh \
    && sed -i 's#OpenCore\.qcow2#OpenCore-nopicker.qcow2#' /home/arch/bin/Launch-nopicker.sh \
    && chmod +x /home/arch/bin/Launch-nopicker.sh

RUN cat > /home/arch/bin/enable-ssh.sh <<'SSH_EOF' && chmod +x /home/arch/bin/enable-ssh.sh
#!/bin/bash
[[ -f /etc/ssh/ssh_host_rsa_key ]] || \
[[ -f /etc/ssh/ssh_host_ed25519_key ]] || \
sudo /usr/bin/ssh-keygen -A
nohup sudo /usr/bin/sshd -D &
SSH_EOF

RUN cat > /home/arch/bin/osx-commands-runner.sh <<'CI_EOF' && chmod +x /home/arch/bin/osx-commands-runner.sh
#!/bin/bash
# Best-effort: guest 内必须已开启远程登录且存在 ${OSX_USERNAME} 账号。
RETRIES="${OSX_SSH_RETRIES:-120}"
INTERVAL=30
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"
PORT="${INTERNAL_SSH_PORT:-10022}"

for i in $(seq 1 "${RETRIES}"); do
    if sshpass -p "${OSX_PASSWORD}" ssh ${SSH_OPTS} -p "${PORT}" \
        "${OSX_USERNAME}@127.0.0.1" "echo ok" >/dev/null 2>&1; then
        printf '%s\n' "[osx-commands] guest SSH is up, executing OSX_COMMANDS"
        IFS=';' read -r -a cmds <<< "${OSX_COMMANDS}"
        rc=0
        for c in "${cmds[@]}"; do
            c="${c#"${c%%[![:space:]]*}"}"
            c="${c%"${c##*[![:space:]]}"}"
            [[ -z "${c}" ]] && continue
            printf '%s\n' "[osx-commands] \$ ${c}"
            sshpass -p "${OSX_PASSWORD}" ssh ${SSH_OPTS} -p "${PORT}" \
                "${OSX_USERNAME}@127.0.0.1" "${c}" || rc=1
        done
        exit "${rc}"
    fi
    sleep "${INTERVAL}"
done
echo "[osx-commands] guest SSH never came up; giving up." >&2
exit 1
CI_EOF

RUN cat > /home/arch/bin/entrypoint.sh <<'ENTRY_EOF' && chmod +x /home/arch/bin/entrypoint.sh
#!/bin/bash
set -euo pipefail

DATA_DIR=/home/arch/OSX-KVM
BIN_DIR=/home/arch/bin
BOOT_DIR=/home/arch/bootdisks
BASE_SYSTEM="${BASESYSTEM_IMAGE:-${DATA_DIR}/BaseSystem.img}"
export IMAGE_PATH="${IMAGE_PATH:-${DATA_DIR}/mac_hdd_ng.img}"
ENV_FILE="${ENV:-/env}"
export KERNEL_ARGS="${KERNEL_ARGS:-}"

cd "${DATA_DIR}"

sudo chown $(id -u):$(id -g) /dev/kvm 2>/dev/null || true
sudo chown -R $(id -u):$(id -g) /dev/snd 2>/dev/null || true
sudo chown -R $(id -u):$(id -g) /dev/bus/usb 2>/dev/null || true
if [[ -n "${SHARE_DIR:-}" && -d "${SHARE_DIR}" ]]; then
    sudo chown $(id -u):$(id -g) "${SHARE_DIR}" 2>/dev/null || true
fi
sudo touch "${ENV_FILE}" 2>/dev/null || true
sudo chown $(id -u):$(id -g) "${ENV_FILE}" 2>/dev/null || true

# 1) 安装器 / 系统盘
if [[ "${NOPICKER}" == true ]]; then
    if [[ ! -f "${IMAGE_PATH}" ]]; then
        make mac_hdd_ng.img DISK_SIZE="${SIZE:-200G}"
    fi
else
    if [[ ! -e "${BASE_SYSTEM}" ]]; then
        printf '%s\n' "No ${BASE_SYSTEM} available, downloading ${SHORTNAME}"
        make DISK_SIZE="${SIZE:-200G}"
        qemu-img convert BaseSystem.dmg -O qcow2 -p -c "${BASE_SYSTEM}"
        rm -f ./BaseSystem.dmg
        if [[ "${BASE_SYSTEM}" != "${DATA_DIR}/BaseSystem.img" ]]; then
            rm -f "${DATA_DIR}/BaseSystem.img"
        fi
        printf '%s\n' "${SHORTNAME}" > "${BASE_SYSTEM}.version"
    else
        if [[ -f "${BASE_SYSTEM}.version" ]] && [[ "$(cat "${BASE_SYSTEM}.version")" != "${SHORTNAME}" ]]; then
            printf '%s\n' "WARNING: ${BASE_SYSTEM} was downloaded for $(cat "${BASE_SYSTEM}.version"), but SHORTNAME=${SHORTNAME}; delete it to switch versions." >&2
        fi
        if [[ ! -f "${IMAGE_PATH}" ]]; then
            make mac_hdd_ng.img DISK_SIZE="${SIZE:-200G}"
        fi
    fi
fi

if [[ ! -f "${IMAGE_PATH}" ]]; then
    printf '%s\n' "ERROR: system disk ${IMAGE_PATH} not found; naked mode requires an existing image." >&2
    exit 1
fi

# 2) launch / bootdisk / plist 选择
if [[ "${NOPICKER}" == true ]]; then
    LAUNCH="${BIN_DIR}/Launch-nopicker.sh"
    PLIST="${MASTER_PLIST_NOPICKER:-${BOOT_DIR}/config-tahoe-nopicker.plist}"
    DEFAULT_BOOTDISK="${BOOT_DIR}/OpenCore-nopicker.qcow2"
else
    LAUNCH="${BIN_DIR}/Launch.sh"
    PLIST="${MASTER_PLIST:-${BOOT_DIR}/config-tahoe-picker.plist}"
    DEFAULT_BOOTDISK="${BOOT_DIR}/OpenCore.qcow2"
fi

if [[ "${GENERATE_UNIQUE}" == true && "${GENERATE_SPECIFIC}" == true ]]; then
    printf '%s\n' "ERROR: GENERATE_UNIQUE and GENERATE_SPECIFIC cannot both be true." >&2
    exit 1
fi

if [[ "${GENERATE_UNIQUE}" == true || "${GENERATE_SPECIFIC}" == true ]]; then
    export BOOTDISK="${BOOTDISK:-${DATA_DIR}/OpenCore/$(basename "${DEFAULT_BOOTDISK}")}"
else
    export BOOTDISK="${BOOTDISK:-${DEFAULT_BOOTDISK}}"
fi

PLIST_ARGS=(--master-plist "${PLIST}")
if [[ -n "${MASTER_PLIST_URL:-}" ]]; then
    PLIST_ARGS=(--master-plist-url "${MASTER_PLIST_URL}")
fi

SERGEN="${DATA_DIR}/Docker-OSX/osx-serial-generator"

if [[ "${GENERATE_UNIQUE}" == true ]]; then
    "${SERGEN}/generate-unique-machine-values.sh" \
        "${PLIST_ARGS[@]}" \
        --count 1 \
        --tsv "${DATA_DIR}/serial.tsv" \
        --bootdisks \
        --width "${WIDTH:-1920}" \
        --height "${HEIGHT:-1080}" \
        --output-bootdisk "${BOOTDISK}" \
        --output-env "${ENV_FILE}" || exit 1
elif [[ "${GENERATE_SPECIFIC}" == true ]]; then
    source "${ENV_FILE}" 2>/dev/null || true
    "${SERGEN}/generate-specific-bootdisk.sh" \
        "${PLIST_ARGS[@]}" \
        --model "${DEVICE_MODEL}" \
        --serial "${SERIAL}" \
        --board-serial "${BOARD_SERIAL}" \
        --uuid "${UUID}" \
        --mac-address "${MAC_ADDRESS}" \
        --width "${WIDTH:-1920}" \
        --height "${HEIGHT:-1080}" \
        --output-bootdisk "${BOOTDISK}" || exit 1
fi

# 3) 可选 CI 钩子
if [[ -n "${OSX_COMMANDS:-}" ]]; then
    nohup "${BIN_DIR}/osx-commands-runner.sh" >/tmp/osx-commands.log 2>&1 &
fi

# 4) 容器 sshd + QEMU
"${BIN_DIR}/enable-ssh.sh"
exec /bin/bash "${LAUNCH}"
ENTRY_EOF

# -----------------------------------------------------------------------------
# Tahoe OpenCore 模板: 从 OSX-KVM 当前 config.plist 生成占位符模板
# -----------------------------------------------------------------------------
RUN python - <<'PY'
import copy
import os
import plistlib

src = "/home/arch/OSX-KVM/OpenCore/config.plist"
outdir = "/home/arch/bootdisks"
with open(src, "rb") as f:
    base = plistlib.load(f)

def make(show_picker, path):
    cfg = copy.deepcopy(base)
    generic = cfg["PlatformInfo"]["Generic"]
    generic["SystemProductName"] = "{{DEVICE_MODEL}}"
    generic["SystemSerialNumber"] = "{{SERIAL}}"
    generic["MLB"] = "{{BOARD_SERIAL}}"
    generic["SystemUUID"] = "{{UUID}}"
    generic["ROM"] = "{{ROM}}"
    cfg["UEFI"]["Output"]["Resolution"] = "{{WIDTH}}x{{HEIGHT}}@32"
    guid = "7C436110-AB2A-4BBB-A880-FE41995C9F82"
    nvram = cfg["NVRAM"]["Add"][guid]
    nvram["boot-args"] = (nvram.get("boot-args", "") + " {{KERNEL_ARGS}}").strip()
    cfg["Misc"]["Boot"]["ShowPicker"] = show_picker
    with open(path, "wb") as f:
        plistlib.dump(cfg, f, fmt=plistlib.FMT_XML)
    with open(path, "rb") as f:
        check = plistlib.load(f)
    assert check["PlatformInfo"]["Generic"]["SystemProductName"] == "{{DEVICE_MODEL}}"
    assert check["PlatformInfo"]["Generic"]["ROM"] == "{{ROM}}"
    assert "{{KERNEL_ARGS}}" in check["NVRAM"]["Add"][guid]["boot-args"]

make(True, os.path.join(outdir, "config-tahoe-picker.plist"))
make(False, os.path.join(outdir, "config-tahoe-nopicker.plist"))
print("Tahoe OpenCore templates generated")
PY

# 预置 vendor MAC 前缀，避免运行时/构建期访问 GitLab。
RUN printf '00:0A:27\tApple, Inc.\n' > /home/arch/OSX-KVM/vendor_macs.tsv

# -----------------------------------------------------------------------------
# stock 引导盘: 受 macOS 26 支持的 MacPro7,1，构建期用 macserial 生成有效序列号
# -----------------------------------------------------------------------------
ARG STOCK_DEVICE_MODEL=MacPro7,1
ARG STOCK_WIDTH=1920
ARG STOCK_HEIGHT=1080
ARG STOCK_KERNEL_ARGS=

RUN KERNEL_ARGS="${STOCK_KERNEL_ARGS}" ./Docker-OSX/osx-serial-generator/generate-unique-machine-values.sh \
        --model "${STOCK_DEVICE_MODEL}" \
        --count 1 \
        --tsv /home/arch/bootdisks/stock.tsv \
        --bootdisks \
        --width "${STOCK_WIDTH}" \
        --height "${STOCK_HEIGHT}" \
        --master-plist /home/arch/bootdisks/config-tahoe-picker.plist \
        --output-bootdisk /home/arch/bootdisks/OpenCore.qcow2 \
        --output-env /home/arch/bootdisks/stock.env \
    && rm -f /home/arch/OSX-KVM/serial_sets-*.csv /home/arch/OSX-KVM/serial_sets-*.tsv

RUN source /home/arch/bootdisks/stock.env \
    && KERNEL_ARGS="${STOCK_KERNEL_ARGS}" ./Docker-OSX/osx-serial-generator/generate-specific-bootdisk.sh \
        --master-plist /home/arch/bootdisks/config-tahoe-nopicker.plist \
        --model "${DEVICE_MODEL}" \
        --serial "${SERIAL}" \
        --board-serial "${BOARD_SERIAL}" \
        --uuid "${UUID}" \
        --mac-address "${MAC_ADDRESS}" \
        --width "${WIDTH:-1920}" \
        --height "${HEIGHT:-1080}" \
        --output-bootdisk /home/arch/bootdisks/OpenCore-nopicker.qcow2

# 构建期验收：模板占位符必须已替换，机型必须是受支持的 MacPro7,1。
RUN qemu-img convert -O raw /home/arch/bootdisks/OpenCore.qcow2 /tmp/stock-picker.raw \
    && mcopy -i /tmp/stock-picker.raw@@1M ::/EFI/OC/config.plist /tmp/stock-picker.plist \
    && ! grep -q '{{' /tmp/stock-picker.plist \
    && grep -q '<string>MacPro7,1</string>' /tmp/stock-picker.plist \
    && grep -A1 '<key>ShowPicker</key>' /tmp/stock-picker.plist | grep -q '<true/>' \
    && rm -f /tmp/stock-picker.raw

RUN qemu-img convert -O raw /home/arch/bootdisks/OpenCore-nopicker.qcow2 /tmp/stock-nopicker.raw \
    && mcopy -i /tmp/stock-nopicker.raw@@1M ::/EFI/OC/config.plist /tmp/stock-nopicker.plist \
    && ! grep -q '{{' /tmp/stock-nopicker.plist \
    && grep -q '<string>MacPro7,1</string>' /tmp/stock-nopicker.plist \
    && grep -A1 '<key>ShowPicker</key>' /tmp/stock-nopicker.plist | grep -q '<false/>' \
    && rm -f /tmp/stock-nopicker.raw

RUN ln -s /home/arch/OSX-KVM/OpenCore /home/arch/OSX-KVM/OpenCore-Catalina || true

# -----------------------------------------------------------------------------
# 运行时环境变量 (全部可用 docker run -e 覆盖)
# -----------------------------------------------------------------------------
ENV USER=arch

ENV LIBGUESTFS_DEBUG=1 \
    LIBGUESTFS_TRACE=1

# macOS 版本与安装器
ENV SHORTNAME=tahoe
ENV BASESYSTEM_IMAGE=/home/arch/OSX-KVM/BaseSystem.img
ENV SIZE=${SIZE}

# 磁盘
ENV IMAGE_PATH=/home/arch/OSX-KVM/mac_hdd_ng.img
ENV IMAGE_FORMAT=qcow2
ENV BASESYSTEM_FORMAT=qcow2

# CPU / 内存 (Tahoe/Sonoma+ 使用 Skylake-Client,-hle,-rtm)
ENV RAM=8
ENV SMP=4
ENV CORES=4
ENV CPU=Skylake-Client
ENV CPUID_FLAGS='kvm=on,vendor=GenuineIntel,+invtsc,vmware-cpuid-freq=on,-hle,-rtm,+ssse3,+sse4.2,+popcnt,+avx,+aes,+xsave,+xsaveopt,check'
ENV KVM='accel=kvm:tcg'

# 显示
ENV DISPLAY=:0.0
ENV WIDTH=1920
ENV HEIGHT=1080

# 网络 / 端口
ENV NETWORKING=vmxnet3
ENV MAC_ADDRESS=52:54:00:09:49:17
ENV INTERNAL_SSH_PORT=10022
ENV SCREEN_SHARE_PORT=5900
ENV ADDITIONAL_PORTS=

# 音频: auto 在无 PCM 时自动使用 none；WSLg 用 pa，Linux 桌面可用 alsa
ENV AUDIO_DRIVER=auto

# 文件夹共享（9p）与 USB 透传
ENV SHARE_DIR=
ENV SHARE_TAG=hostshare
ENV SHARE_SECURITY_MODEL=passthrough
ENV USB_DEVICE=

# QEMU 扩展
ENV EXTRA=
ENV BOOT_ARGS=
ENV BOOTDISK=

# 启动行为
ENV NOPICKER=false
ENV GENERATE_UNIQUE=false
ENV GENERATE_SPECIFIC=false

# 本地 Tahoe OpenCore 模板；设置 MASTER_PLIST_URL 可覆盖为远程 plist
ENV MASTER_PLIST=/home/arch/bootdisks/config-tahoe-picker.plist
ENV MASTER_PLIST_NOPICKER=/home/arch/bootdisks/config-tahoe-nopicker.plist
ENV MASTER_PLIST_URL=
ENV KERNEL_ARGS=

# CI: guest 内自动执行命令 (分号分隔; 需 guest 已开启 SSH)
ENV OSX_COMMANDS=
ENV OSX_USERNAME=user
ENV OSX_PASSWORD=alpine
ENV OSX_SSH_RETRIES=120

# 兼容上游
ENV ENV=/env

EXPOSE 10022 5900 5999

VOLUME ["/tmp/.X11-unix"]

HEALTHCHECK --interval=30s --timeout=15s --start-period=2400s --retries=3 \
    CMD pgrep -f "qemu-system-x86_64" >/dev/null || exit 1

CMD ["/home/arch/bin/entrypoint.sh"]
