## Docker 运行

镜像由 GitHub Actions 自动构建并推送到 GitHub Container Registry，支持
`amd64`、`386`、`arm/v6`、`arm/v7`、`arm64`、`riscv64`、`ppc64le`、`s390x`。

```zsh
$ docker run -d --name zju-connect \
    -v $PWD/config.toml:/data/config.toml:ro \
    -v $PWD/data:/data/state \
    -p 1080:1080 -p 8888:8888 \
    --restart unless-stopped \
    ghcr.io/forstnova-arknights/zju-connect:latest
```

> 国内（尤其 arm 小机器）直连 `ghcr.io` 可能很慢，可以用南京大学镜像源：
> `ghcr.nju.edu.cn/forstnova-arknights/zju-connect:latest`

### 容器内路径

| 路径 | 用途 |
| --- | --- |
| `/data/config.toml` | 配置文件，建议**只读**挂载 |
| `/data/state/` | 运行时状态目录，`client_data.json` 落在这里，需要可写 |

镜像内 `ENTRYPOINT` 已经带上 `--config /data/config.toml`，因此不需要在
`docker run` 里重复指定。

### 端口

| 端口 | 协议 | 鉴权 |
| --- | --- | --- |
| `1080` | SOCKS5 | 支持，见 `socks_user` / `socks_passwd` |
| `8888` | HTTP 代理 | **不支持** |

> ⚠️ HTTP 代理没有鉴权（`service/http.go` 的 `ServeHTTP` 不接受用户名密码），
> 把它映射到 `0.0.0.0` 相当于把通往内网的代理开放给整个局域网。
> 单机自用建议只绑回环：`-p 127.0.0.1:8888:8888`。

### 不需要特权

`tun_mode` 默认为 `false`，走的是用户态代理，因此**不需要** `--cap-add NET_ADMIN`，
也**不需要**映射 `/dev/net/tun`。`root` 权限同样不是必需的。

## Docker Compose

仓库根目录的 `docker-compose.yml` 可直接使用：

```zsh
$ mkdir -p state
$ cp config.example.toml config.toml && chmod 600 config.toml
$ $EDITOR config.toml
$ docker compose up -d
$ docker compose logs -f
```

也可以把配置直接用 [configs top-level elements](https://docs.docker.com/compose/compose-file/08-configs/)
写进 `docker-compose.yml`：

```yaml
services:
  zju-connect:
    image: ghcr.io/forstnova-arknights/zju-connect:latest
    container_name: zju-connect
    restart: unless-stopped
    ports:
      - "1080:1080"
      - "127.0.0.1:8888:8888"
    configs:
      - source: zju-connect-config
        target: /data/config.toml
    volumes:
      - zju-connect-data:/data/state

configs:
  zju-connect-config:
    content: |
      username = ""
      password = ""
      client_data_file = "/data/state/client_data.json"
      # other configs ...

volumes:
  zju-connect-data:
```

### aTrust 协议登录和数据持久化

aTrust 协议客户端需要在容器内持久化登录凭据（`client_data.json`），
所以务必把 `/data/state` 挂到卷或宿主目录上，否则容器重建后要重新登录。

如果服务端支持 CAS 自动登录（例如 `auth_type = "auth/cas"` 配合
`cas_auto_login = true`），初次登录也完全无需人工介入：

```toml
auth_type      = "auth/cas"
login_domain   = "cas07533"
cas_auto_login = true
totp_secret    = "<TOTP 密钥>"
```

反之，若走 `auth/psw`（本地密码认证），服务端可能要求人工完成图形验证码。
此时应当用 `network_mode: host` 和交互模式临时运行：

```zsh
docker compose run --rm -it zju-connect
```

程序会启动一个临时 HTTP 服务器显示验证码，端口号由系统随机分配。
请从日志中查看实际地址：

```
2026/03/25 09:10:46 Captcha server started at http://127.0.0.1:40855
2026/03/25 09:10:46 Failed to open browser: exec: "xdg-open": executable file not found in $PATH. Please visit: http://127.0.0.1:40855
```

> 注意：在玩客云这类没有图形环境的小机器上，容器内没有 `xdg-open`，
> 浏览器不会自动打开。可以把该端口转发到本机再人工访问。

访问日志中显示的地址完成图形验证码验证后，程序会提示输入短信验证码：

```
2026/03/25 09:10:57 SMS message sent successfully: 验证码已发送到您的手机：***, 请查收！
zju-connect-1  | 2026/03/25 09:11:50 Please enter the SMS verification code:
```

出现上述提示后，请输入收到的短信验证码并回车。登录成功后会显示如下日志：

```
2026/03/25 09:12:12 Client data saved to /data/state/client_data.json
2026/03/25 09:12:12 VPN client started
```

完成登录后按 `Ctrl+C` 退出容器，之后就可以使用 `docker compose up -d` 正常启动了，
登录状态会被持久化在卷中，无需重复验证。

## 构建自定义镜像

```zsh
$ docker buildx build --platform linux/arm/v7 -t zju-connect:local .
```

本仓库的 `Dockerfile` 使用 `--platform=$BUILDPLATFORM` 配合 Go 原生交叉编译，
所以构建多架构镜像**不需要 QEMU**，速度很快。
