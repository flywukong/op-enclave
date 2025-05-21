# === Base Bootstrap 镜像：用于构建 Linuxkit initrd、拉取 bootstrap bzImage ===
FROM ghcr.io/mdehoog/aws-nitro-enclaves-sdk-bootstrap@sha256:6e5e53bd47370dbc1920208e93d222533a36f9f5dc85615591cbfe56a03312b0 AS bootstrap


# === Golang builder：使用 alpine + musl-dev 进行静态编译 ===
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache musl-dev gcc g++ make

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1
ENV GOOS=linux
ENV GOARCH=amd64

# ✅ 静态编译 Go 程序（适配 Nitro Enclaves 无 libc 环境）
RUN go build -ldflags="-linkmode external -extldflags -static" -o bin/enclave ./cmd/enclave

# 拉入 init/user ramdisk 配置
COPY eif eif/
COPY --from=bootstrap /build/out bootstrap

# 安装并使用 linuxkit 构建 initrd 镜像
RUN go install github.com/linuxkit/linuxkit/src/cmd/linuxkit@270fd1c5aa1986977b31af6c743c6a2681f67a29
RUN linuxkit build --format kernel+initrd --no-sbom --name init-ramdisk ./eif/init-ramdisk.yaml
RUN linuxkit build --format kernel+initrd --no-sbom --name user-ramdisk ./eif/user-ramdisk.yaml


# === 构建 EIF 镜像（使用官方 rust 编译 eif_build 工具）===
FROM rust:1.80.1 AS eif

RUN mkdir /build
WORKDIR /build

# 拉取 aws-nitro-enclaves-image-format 工具
ENV REPO=https://github.com/aws/aws-nitro-enclaves-image-format.git
ENV COMMIT=483114f1da3bad913ad1fb7d5c00dadacc6cbae6
RUN git init && \
    git remote add origin $REPO && \
    git fetch --depth=1 origin $COMMIT && \
    git reset --hard FETCH_HEAD

RUN cargo build --all --release

# 拷贝必要的构建资源
COPY eif/cmdline-x86_64 cmdline
COPY --from=bootstrap /build/out bootstrap
COPY --from=builder /build/init-ramdisk-initrd.img .
COPY --from=builder /build/user-ramdisk-initrd.img .

# ✅ 构建最终的 EIF 镜像
RUN ./target/release/eif_build \
    --kernel bootstrap/bzImage \
    --kernel_config bootstrap/bzImage.config \
    --cmdline "$(cat cmdline)" \
    --ramdisk init-ramdisk-initrd.img \
    --ramdisk user-ramdisk-initrd.img \
    --output eif.bin


# === 最终 stage：产出 eif.bin ===
FROM busybox

RUN mkdir /build
WORKDIR /build

COPY --from=eif /build/eif.bin .
