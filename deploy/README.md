# gotorrent 部署

## Docker

```bash
# 构建
docker build -t gotorrent:local .

# 启动(推荐用 compose)
docker compose up -d

# 或手动 docker run
docker run -d --name gotorrent \
    -p 6800:6800 \
    -v ./downloads:/downloads \
    -v ./config:/config \
    -e TZ=Asia/Shanghai \
    gotorrent:local
```

控制面地址: http://localhost:6800/ui

## 裸机 systemd

```bash
# 编译
go build -o gotorrent .

# 一键部署(创建用户、安装二进制、写 env 文件、注册 service 并启动)
sudo ./deploy/install.sh

# 查看状态
systemctl status gotorrent
journalctl -u gotorrent -f
```

令牌存放在 `/etc/gotorrent/gotorrent.env`,权限 600,仅 root 可读。
