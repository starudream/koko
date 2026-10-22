# 通用 SSH TCP 转发

## 基线与配置

长期维护入口为 `custom/tcp-forward-v4.10.19`，发布时从指定官方稳定 Tag 重新获取源码并
重放功能补丁。首次验收基线为官方 `v4.10.19`
（`1a6befc9c75d1e1726787d7635fa2aee2593ab05`），对应 `jumpserver/jms_all:v4.10.19`。
保留 `module github.com/jumpserver/koko`；功能补丁不更改依赖，构建候选按条件补入
缺失的上游依赖修复，例如 #2085。版本升级流程见
[长期维护说明](custom-release-maintenance.md)。
每个二进制应与同版本 All-in-One 配套；真实 Core、DataGrip 和数据库仍需现场验收。

只新增一个参数，默认开启：

```yaml
ENABLE_GENERIC_TCP_FORWARD: true
```

替换二进制并重启 KoKo 后，无需重建容器或添加配置即可使用默认值。
可通过配置文件或环境变量显式设置 `ENABLE_GENERIC_TCP_FORWARD=false` 关闭。
加载顺序沿用本版本：
默认值 → 环境变量 → 配置文件，配置文件中显式出现的字段优先。
默认配置文件为进程工作目录中的 `config.yml`，可通过原有 `-f` 参数选择。
Supervisor 启动的 All-in-One KoKo 工作目录为 `/opt/koko`。
在另一个 `docker exec` Shell 中临时 export 不会改变已运行进程的环境。

按本次确认的需求，不设置目的地址白名单、端口白名单或新的并发配置。
客户端可以指定 IP 或主机名及 TCP 端口，连接由 **Core 授权的 SSH 资产**发起，
目标范围等于该资产实际可达的 TCP 网络。KoKo 不直接访问这些 TCP 目标。
原有 `SSH_MAX_SESSIONS` 仍按上游语义限制入站连接的 Channel 数量。

纯转发不要求开启 `ENABLE_LOCAL_PORT_FORWARD` 或 `ENABLE_VSCODE_SUPPORT`。
新开关关闭时沿用旧实现。已存在的 VS Code 请求在两个旧开关开启时继续走原路径；
普通 Session、SFTP 和 `ssh -R` 的处理器没有修改。

## 授权与生命周期

- 沿用 KoKo 的 Core 用户认证和 `用户@账号@资产` 直连格式，也支持现有 `JMS-`
  Connect Token 登录。不要将 Token、密码或资产真实信息提交到仓库。
- 非 Token 登录复用已有资产查询、唯一账号选择及 `buildConnectToken`；资产或账号
  不唯一、无授权、协议不是 SSH 时拒绝。非交互式连接无法完成的审批直接拒绝。
- 校验 Token 用户、连接权限、授权到期时间及托管账号。不支持 `@INPUT`、`@USER`、
  匿名账号、无凭据账号或需要 `su` 切换的账号。
- 首个 `direct-tcpip` Channel 才初始化资产连接，无需 Shell、PTY 或 SSH Session。
  同一入站连接的并发 Channel 只初始化一次；不同连接不共用状态或出站 SSH Client。
  不接入全局 `REUSE_CONNECTION` 缓存，断开时可直接释放独占 Client。
- 创建 Core tunnel 会话并加入原有 session 管理器。管理员终止、权限过期或锁定会
  取消整个转发上下文。锁定按终止处理，需要重新连接。已有的每五分钟 Token 权限
  检查继续适用，因此权限撤销的发现沿用该周期；不会新增权限轮询机制。
- Core `check` 返回的 `expired` 可能仅表示一次性登录令牌已消费；持续权限使用
  `code=perm_ok`、Token 中的 `expire_at` 和会话控制指令判断。
- 授权到期、入站连接断开、后端 SSH 断开都会关闭已建 Channel 和出站连接。
  初始化失败或会话终止后，同一连接不能重新初始化；客户端需重新建立 SSH 连接。
  Core HTTP 请求沿用原有超时，出站 SSH 建连、握手和 TCP Dial 使用 `SSH_TIMEOUT`。
- 双向复制支持 EOF/half-close，不记录数据库数据、SQL、密码或密钥。

## OpenSSH 与 DataGrip 验收

在有授权的测试环境确认开关开启，先使用测试端口 `2223` 或测试容器，避免与现有
`2222` 冲突。以下命令中的值均为占位符。

```bash
ssh -vvv -N -o ExitOnForwardFailure=yes \
  -L 127.0.0.1:13306:127.0.0.1:3306 \
  -p 2223 -l '<JMS_USER>@<ASSET_ACCOUNT>@<ASSET_IP>' <JUMPSERVER_HOST>

mysql --host=127.0.0.1 --port=13306 --user='<MYSQL_USER>' --password
```

`127.0.0.1:3306` 指资产的回环地址。必须实际连接本地 `13306` 并确认 MySQL
握手、查询、断开及重连，`ExitOnForwardFailure=yes` 只证明本地监听建立。
验证无资产授权、无账号授权、需审批、授权到期和管理员终止均会拒绝或断开。

DataGrip 可在 General 中配置 `127.0.0.1:13306` 使用上述本地隧道。
原生 SSH Tunnel 的 SSH 主机为 JumpServer，端口为测试入口，用户名使用同一直连格式；
数据库地址为从 SSH 资产看到的 MySQL 地址。现场验证连接、查询、断开和重连。

动态 SOCKS 使用相同的 `direct-tcpip` 路径：

```bash
ssh -N -D 127.0.0.1:1080 -p 2223 \
  -l '<JMS_USER>@<ASSET_ACCOUNT>@<ASSET_IP>' <JUMPSERVER_HOST>
```

本实现不限制 SOCKS 目的地址；网络范围同资产可达范围。`ssh -R` 继续使用原有
`ENABLE_REVERSE_PORT_FORWARD` 及处理器，本补丁不改变其行为。

## CI 与本地验证

复用 `.github/workflows/build-ghcr-image.yml`，名称为 `Build Linux amd64 binary`。
维护分支的 push 触发使用清单中的默认 Tag；手动运行可指定 `UPSTREAM_TAG`、
`PATCH_REVISION` 和补丁维护 `BRANCH`。版本由 Tag 与序号组合，不再固定为 v4.10.19。
完整输入、升级步骤及本地测试命令见 [长期维护说明](custom-release-maintenance.md)。

构建流程为：官方 Tag 独立检出 → 严格重放功能补丁 → 按需补上游依赖修复 →
原版 UI → 强制 Linux amd64 构建 → 打包。
CI 按候选源码的 `go.mod` 选择 Go，UI 仍是上游嵌入二进制的真实内容。
CI 保留维护工具测试，不自动执行 Go 转发测试、race 或 vet；这些检查按需手动运行。
补丁冲突、模块 replacement 或构建失败需要人工适配。

Artifact 仍为 `koko-linux-amd64`，包名如 `koko-v4.10.19-fwd.1-linux-amd64.tar.gz`。
其中只有 `koko`、`BUILD_INFO.txt`、`SHA256SUMS`，不发布 GHCR 镜像、不部署。
构建信息包含官方 Tag/SHA、维护提交、补丁集及候选差异指纹、实际 `x/crypto` 版本
和各依赖修复状态。只有通过校验的静态 amd64 ELF 才能打包。

自动测试使用本地 SSH 连接和内存中的 Core API 响应，覆盖无 Session、双向传输、
half-close、并发首建、原有 Channel 上限、断开回收、权限到期/撤销及授权失败。
维护工具另验证安全重放、冲突停止、目录保护和条件依赖修复。
这些检查不等同于真实 Core、MySQL、DataGrip 或 All-in-One 运行时验收。

原版 Makefile 的 `build` 不是 PHONY，工具使用 `make -B build` 强制重建。
完整相关包 golangci-lint 仍存在上游已有问题，应区分本次差异检查与全包检查结果。

## 手动替换与回滚

以下以首次基线为例，仅供操作者执行。本补丁不自动操作容器。先确认容器的镜像为
`jumpserver/jms_all:v4.10.19`、架构为 amd64，记录原镜像 ID/Digest，备份原
二进制、实际配置以及既有持久化卷。不要覆盖组件注册密钥、账号或数据库。

在解包目录校验文件并备份，`CONTAINER` 需替换为实际容器名：

```bash
sha256sum -c SHA256SUMS
file koko
container='<CONTAINER>'
backup_dir="koko-backup-$(date +%Y%m%d-%H%M%S)"
mkdir -m 700 "$backup_dir"
docker inspect --format '{{.Image}}' "$container" > "$backup_dir/image-id.txt"
docker cp "$container:/opt/koko/koko" "$backup_dir/koko"
docker cp "$container:/opt/koko/config.yml" "$backup_dir/config.yml"
chmod 600 "$backup_dir/config.yml"
docker cp ./koko "$container:/opt/koko/koko.new"
```

若当前没有 `/opt/koko/config.yml`，按现有 `-f` 参数或环境变量方式找到实际配置，
备份后再继续；不要用示例配置覆盖现有配置。新开关默认开启，无需添加配置；
若环境变量或配置文件已显式设为 false，需删除该设置或改为 true。

官方 All-in-One 的 Supervisor 程序名为 `koko`，工作目录为 `/opt/koko`。
先确认以下 `status` 命令可用；容器中的 Supervisor 配置路径以实际安装为准。

```bash
docker exec "$container" supervisorctl status koko
docker exec "$container" supervisorctl stop koko
docker exec "$container" sh -eu -c 'chmod 755 /opt/koko/koko.new; mv /opt/koko/koko.new /opt/koko/koko'
docker exec "$container" supervisorctl start koko
docker exec "$container" supervisorctl status koko
docker logs --since 2m "$container"
```

替换会中断该 KoKo 的现有连接。确认进程持续 RUNNING、Core 终端在线，再进行
上述真实握手和读写验收，并回归普通 SSH、SFTP、VS Code（若启用）和 `-R`。
无需重启整个 All-in-One，也无需执行数据库迁移。容器重建会丢失手工替换，
应保留精确版本的包、校验和及手工操作记录。

启动或功能异常时恢复原二进制和原配置：

```bash
docker exec "$container" supervisorctl stop koko
docker cp "$backup_dir/koko" "$container:/opt/koko/koko"
docker cp "$backup_dir/config.yml" "$container:/opt/koko/config.yml"
docker exec "$container" chmod 755 /opt/koko/koko
docker exec "$container" supervisorctl start koko
docker exec "$container" supervisorctl status koko
```

若需要重建容器，使用记录的原镜像 ID/Digest，保留原持久化卷；不要删除卷。

## 后续同步

维护分支保存转发实现、配置及测试，并导出到 `custom/patches/`，上游依赖修复
单独登记在清单中。升级时工具直接获取新官方 Tag，无需在 Fork 保留上游跟踪分支；
核对 API 契约后，按需选择验证项目。
已包含的依赖修复自动跳过；功能补丁冲突则人工适配，保留旧维护提交用于复现和回滚。
官方原生支持并通过同一验收后，再停用自定义补丁。详见
[版本升级与退役流程](custom-release-maintenance.md)。
