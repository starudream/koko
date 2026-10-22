# 跟进官方版本与自定义补丁退役

## 维护边界

本项目维护默认关闭、经过 Core 认证和资产授权的无 Session SSH TCP 转发补丁，
持续适配官方稳定 Release，直到官方实现满足同一验收要求。
发布产物按“官方 Tag + 功能补丁 + 缺失的上游修复”生成，不固定在 v4.10.19。
当前官方 Tag 的兼容性必须逐版验证，未知版本的冲突或测试失败会阻止打包。

`dev` 继续跟踪官方。`custom/tcp-forward-v4.10.19` 保存维护工具和补丁；此前的
`feature/generic-tcp-forward-v4.10.19` 分支保留为旧版本回滚点，不改写其已推送历史。
首次默认基线仍为 v4.10.19，真实 Core、DataGrip 和 All-in-One 验收独立进行。

## 新版本发布后

1. 查看官方稳定 Release，例如 `gh api repos/jumpserver/koko/releases/latest --jq .tag_name`。
   按现网将要使用的 All-in-One 版本选择对应 KoKo Tag。工具只接受 `vX.Y.Z` 稳定 Tag，
   不接受 `dev`、RC 或任意 ref。
2. 使用当前维护工具指定该 Tag，准备独立候选目录。它会创建本地 `custom/vX.Y.Z`
   分支、重放功能补丁，记录官方 SHA 和补丁来源。
3. 核对 Core/SDK 认证授权、`buildConnectToken`、`buildSSHClientOptions`、SSH Client、
   session 控制及配置加载契约。应用成功也需要编译和行为测试，不能只看补丁是否合上。
4. 使用目标 Tag 对应的 Go 工具链检查条件依赖修复。#2085 在目标版本缺失时补入，
   已包含修复或依赖版本更高时跳过。相关 `x/*` 依赖变化需要随候选差异一起审查。
5. 运行维护工具测试、转发测试、race、vet、原版 UI 和静态二进制构建，再到匹配版本
   的测试环境验证 SSH/SFTP、VS Code、反向转发和 DataGrip/MySQL。
6. 验收通过后保存维护补丁提交、候选差异及二进制校验和。若发布新的补丁序号，使用
   `vX.Y.Z-fwd.N`。当前维护分支可更新默认 Tag；旧版本仍可使用原维护提交复现。

如果补丁失败，工具不会自动使用旧版本源码代替新 Tag，也不会自动解决冲突。
检查保留的候选目录，在独立适配分支修改功能文件及补丁，再用全新的目录重试。
候选目录中的 `git diff HEAD` 可同时审查已暂存的功能补丁和未暂存的依赖修复。
对候选目录改换 HEAD、修改维护工具/补丁集、检查依赖后再次改动模块图，都会要求
重新准备候选，避免构建来源与记录不一致。

## 本地准备与构建

以下以当前已发布的 v4.10.19 为例；新版本发布后替换 `--tag` 即可。
依赖 Git、Python 3、目标源码要求的 Go、Node.js 22 和 Yarn 1.22.22。
未来版本若改变 Node、CGO、UI 或 Makefile 构建要求，应先适配并验证构建流程。

```bash
python3 utils/custom_release.py prepare \
  --tag v4.10.19 --revision 1 --destination build/candidate-v4.10.19

# 先确认 Go 版本满足候选目录 go.mod 的要求
python3 utils/custom_release.py dependencies --source build/candidate-v4.10.19

yarn --cwd build/candidate-v4.10.19/ui install --frozen-lockfile --non-interactive --network-concurrency 4
yarn --cwd build/candidate-v4.10.19/ui build

PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s utils -p 'test_custom_release.py'
(
  cd build/candidate-v4.10.19
  GOMAXPROCS=2 go test -mod=readonly -p 2 -timeout 120s ./pkg/config ./pkg/handler ./pkg/srvconn
  CGO_ENABLED=1 GOMAXPROCS=2 go test -mod=readonly -race -p 2 -timeout 120s \
    ./pkg/config ./pkg/handler ./pkg/srvconn -run 'TestGeneric|TestNewSSHClientContext'
  GOMAXPROCS=2 go vet -mod=readonly -p 2 ./pkg/config ./pkg/handler ./pkg/sshd ./pkg/srvconn
  git diff HEAD --stat
)
python3 utils/custom_release.py build --source build/candidate-v4.10.19 --destination build/packages
```

准备工具不覆盖已有目录，打包工具不覆盖已有包。修订代码后使用新候选目录和新
输出目录或补丁序号，保留前一个可回滚产物。编译使用上游 `make -B build` 强制
重建，不执行目标程序；整个命令及子进程有超时，Go 并发默认限制为 2。

## Actions

继续复用 `.github/workflows/build-ghcr-image.yml`，名称为 `Build Linux amd64 binary`。
推送 `feature/generic-tcp-forward*` 或 `custom/**` 维护分支时按清单默认值构建。
手动运行可以指定：

| 输入 | 含义 |
| --- | --- |
| `BRANCH` | 补丁维护分支/提交，留空使用本次 workflow ref |
| `UPSTREAM_TAG` | 官方稳定 Tag，留空读取 `custom/manifest.json` |
| `PATCH_REVISION` | 正整数补丁序号，留空读取清单 |

这里不再使用旧的 `VERSION` 参数，包版本由官方 Tag 和补丁序号组合，避免标签和
源码版本不一致。提交并推送维护分支后，可按下面的方式触发：

```bash
gh workflow run build-ghcr-image.yml --repo starudream/koko \
  --ref custom/tcp-forward-v4.10.19 \
  -f BRANCH=custom/tcp-forward-v4.10.19 \
  -f UPSTREAM_TAG=v4.10.19 \
  -f PATCH_REVISION=1
```

GitHub 的手动入口发现依赖默认分支的工作流注册；本仓库已通过同文件名的 push
工作流完成注册。若更换到新的 Fork 后列表为空，先推送匹配上述规则的维护分支。
不要将功能变更合并到 `dev` 来规避入口问题。

Actions 使用候选源码的 `go.mod` 选择 Go，所有应用测试、UI 和构建都在该源码目录
中执行。候选源码自带的 workflow 不会代替维护分支中的构建流程。
候选目录用于审查，不会自动提交、推送或创建升级 PR。需要 PR 时先明确目标基线分支，
再经人工审查发布，避免将旧发布基线的整个历史合入跟踪最新上游的 `dev`。

最终仍只有 `linux/amd64` 二进制包 Artifact `koko-linux-amd64`，没有 GHCR 发布或部署。
包内为 `koko`、`BUILD_INFO.txt`、`SHA256SUMS`。构建信息记录官方 Tag/SHA、维护提交、
补丁集指纹、候选源码差异指纹、实际 `x/crypto` 版本和各条件修复的应用状态。
未提交维护代码的本地构建明确标记 `CUSTOM_SOURCE_DIRTY=true`；发布应使用已审查提交。
工具还验证输出为没有动态加载器的 amd64 ELF，遇到未来上游构建方式变化则停止适配。

## 官方原生支持后退役

“增加某个开关”或“能够使用 VS Code”不是退役依据。确认官方版本无需 Shell、PTY
或普通 SSH Session，就能用标准 `ssh -N -L` 和 DataGrip 建立 TCP 转发，且保留
Core 身份认证、唯一资产绑定和账号授权，并通过拒绝未授权连接、断线、权限到期及
普通 SSH/SFTP/VS Code/-R 回归验收后，才将该版本切换为官方二进制。

若官方行为满足已确认的需求，停止向该版本及后续版本应用自定义转发补丁，停用
自定义发布流程，保留历史分支、对应包和回滚记录。工具不会因补丁冲突或发现类似
配置字段就自动宣布功能等价。旧版本仍按原维护提交和对应补丁继续复现。
