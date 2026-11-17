# 反向代理

## 网页 HTTPS

将以下配置放入已有的 Nginx HTTPS `server` 块：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

后台「系统设置」的面板对外地址填写实际 HTTPS 地址。

代理在其他服务器时，在 `/etc/nekopass/control.env` 将 `NEKOPASS_TRUSTED_PROXIES` 设置为代理来源 CIDR，多个网段用逗号分隔，然后重启面板。单个 IPv4 使用 `/32`，IPv6 使用 `/128`；这里应填写主控实际看到的代理地址。

网页和 API 应使用同一个对外地址。当前未提供浏览器直接跨域调用独立 API 域名的部署模式。

## 节点控制连接

生产环境可用已有公共 CA 证书，或让 Nginx 终止 TLS。使用 Nginx 时，安装器选择 `proxy` 模式，在独立域名的 HTTPS `server` 中配置公共 CA 证书并启用 HTTP/2：

节点运行时通过 gRPC 双向流接口 `/apacrerx.v1.Control/Connect` 与主控通信。系统设置中的 Agent 通信地址只填写域名和可选端口，Agent 会自动使用该接口路径。

```nginx
location / {
    grpc_pass grpc://127.0.0.1:9443;
    grpc_read_timeout 3600s;
    grpc_send_timeout 3600s;
}
```

此处 `9443` 为安装器选择的节点后端监听端口。系统设置的「Agent 通信地址」填写节点能够访问的 Nginx 地址；例如 Nginx 对外使用 443 时，填写 `https://agent.example.com` 即可，也可写成 `https://agent.example.com:443`。节点安装命令使用此通信地址。未配置主控本地证书时，控制端口保持明文后端，由 Nginx 终止 TLS。

系统设置选择 HTTP 并保存，会立即关闭主控控制端口的 TLS；原本仅监听本机的代理后端会开放直接连接。节点通信地址可填写 `http://IP:9443`；这是独立 gRPC 端口，不是网页 `8080`。主控重启后保留选择，网页 HTTPS 配置不受影响。

`127.0.0.1:9443` 只能用于节点与主控在同一台机器的情况；远程节点必须填写它能访问的主控域名或 IP，不能使用本机回环地址。

在线的 v0.14.1 或更新节点会自动重连到新地址，并持久保存；离线或旧版本节点需执行新的安装命令。HTTP 直连需要防火墙及安全组放行控制端口。

```bash
sudo nginx -t
sudo systemctl reload nginx
```
