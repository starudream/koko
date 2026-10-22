# 长期补丁维护入口

维护分支为 `custom/tcp-forward-v4.10.19`。分支中的 Go 文件用于开发和维护转发补丁，
发布时由工具重新获取官方 Tag，构建的是该 Tag 加补丁后的独立源码目录。
维护分支的历史基线和名字不决定最终二进制版本。

## 文件职责

- `manifest.json`：默认官方 Tag、自定义补丁序号、已核对的基线 SHA、功能补丁清单、
  条件依赖修复清单。默认 Tag 是当前验收基线，不会随官方发布静默切换。
- `patches/generic-tcp-forward.patch`：只含转发配置、入口、实现和测试，不携带 CI、
  Dockerfile、Go 依赖或部署版本字符串。
- `../utils/custom_release.py`：准备独立源码、检查并补齐依赖修复、强制构建和打包。

完整流程见 [版本升级与退役说明](../docs/custom-release-maintenance.md)。

## 功能补丁与上游修复

转发补丁使用严格的 `git apply --check`，通过后才应用到候选检出的暂存区。
上下文、协议或函数签名不匹配时停止，保留失败目录供定位。工具不自动解决冲突，
也不根据配置字段或关键字猜测官方已经实现功能。

#2085 单独登记为依赖修复：检查实际模块图中的 `golang.org/x/crypto`，低于
`v0.56.0` 才执行 `go get golang.org/x/crypto@v0.56.0`，由该版本的 Go 工具解析必要的
传递依赖。已达到该版本或更高版本时跳过。发现模块 replacement 则停止人工核对。
这只在新建候选目录中进行，维护工作区的 `go.mod` 和暂存状态保持不变。

## 更新功能补丁

当前补丁导出基线为官方 `v4.10.19`。修改这些已跟踪的实现文件后，重新导出：

```bash
git diff --binary --full-index v4.10.19 -- \
  config_example.yml \
  pkg/config/config.go pkg/config/generic_tcp_forward_test.go \
  pkg/handler/server_ssh.go pkg/handler/server_ssh_generic_forward.go \
  pkg/handler/server_ssh_generic_forward_test.go \
  pkg/srvconn/ssh_context.go pkg/srvconn/ssh_context_test.go \
  > custom/patches/generic-tcp-forward.patch
```

若新增了其他功能文件，审查并将它加入暂存区和导出清单，避免未跟踪文件被遗漏。
不要把构建目录、实际配置、凭据或不相关改动导出到补丁。
准备候选版本前，工具会在维护检出执行反向检查，防止功能代码已改而补丁没有更新。

后续适配新接口时，在新维护分支上更新实现及其补丁；保留已验证旧分支/提交，
旧版本二进制继续使用对应补丁提交构建。升级默认基线时同步更新
`validated_baseline` 的 Tag 和真实提交 SHA，避免同名 Tag 移动后静默替换来源。
