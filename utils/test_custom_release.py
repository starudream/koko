import argparse
from contextlib import redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("custom_release", Path(__file__).with_name("custom_release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class CustomReleaseTests(unittest.TestCase):
    def test_prepare_replays_patch_without_touching_maintenance_index(self):
        with tempfile.TemporaryDirectory() as temp:
            base = Path(temp)
            upstream, maintenance, candidate = (base / name for name in ("upstream", "maintenance", "candidate"))
            upstream.mkdir()
            release.run(["git", "init", "--quiet", str(upstream)])
            (upstream / "go.mod").write_text("module github.com/jumpserver/koko\n\ngo 1.26\n")
            (upstream / "feature.txt").write_text("upstream\n")
            release.run(["git", "add", "."], upstream)
            commit = ["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "-m", "upstream"]
            release.run(commit, upstream)
            sha = release.run(["git", "rev-parse", "HEAD"], upstream)
            release.run(["git", "tag", "v4.10.19"], upstream)
            release.run(["git", "clone", "--quiet", str(upstream), str(maintenance)])
            (maintenance / "feature.txt").write_text("forwarding\n")
            changes = release.run(["git", "diff", "--binary"], maintenance) + "\n"
            (maintenance / "custom").mkdir()
            (maintenance / "custom/feature.patch").write_text(changes)
            (maintenance / ".github/workflows").mkdir(parents=True)
            (maintenance / ".github/workflows/build-ghcr-image.yml").write_text("name: build\n")
            data = {
                "upstream_repository": "https://github.com/jumpserver/koko.git",
                "default_tag": "v4.10.19", "patch_revision": 2,
                "validated_baseline": {"tag": "v4.10.19", "sha": sha},
                "patches": ["feature.patch"], "dependency_fixes": [],
            }
            manifest = maintenance / "custom/manifest.json"
            manifest.write_text(json.dumps(data))
            before = release.run(["git", "status", "--porcelain"], maintenance)
            real_run = release.run

            def local_run(args, cwd=None):
                if args[:4] == ["git", "remote", "add", "upstream"]:
                    args = args[:-1] + [str(upstream)]
                return real_run(args, cwd)

            # 指纹需包含真实工具源码；测试只替换补丁清单和 Git 远端。
            with patch.object(release, "ROOT", maintenance), patch.object(release, "MANIFEST", manifest), \
                    patch.object(release, "patch_fingerprint", return_value="test-fingerprint"), \
                    patch.object(release, "run", side_effect=local_run), redirect_stdout(io.StringIO()):
                release.prepare(argparse.Namespace(tag=None, revision=None, destination=candidate))
                state = release.read_state(candidate)
                self.assertEqual(state["upstream_sha"], sha)
                self.assertEqual(state["version"], "v4.10.19-fwd.2")
                self.assertEqual((candidate / "feature.txt").read_text(), "forwarding\n")
                self.assertIn("feature.txt", real_run(["git", "diff", "--cached", "--name-only"], candidate))
                self.assertEqual(real_run(["git", "status", "--porcelain"], maintenance), before)
                self.assertEqual((upstream / "feature.txt").read_text(), "upstream\n")
                # 新版本上下文改变时严格拒绝，保留候选目录供检查。
                (upstream / "feature.txt").write_text("official interface changed\n")
                real_run(["git", "add", "feature.txt"], upstream)
                real_run(commit, upstream)
                real_run(["git", "tag", "v4.10.20"], upstream)
                conflict = base / "conflict"
                with self.assertRaises(subprocess.CalledProcessError):
                    release.prepare(argparse.Namespace(tag="v4.10.20", revision=1, destination=conflict))
                self.assertFalse(release.state_path(conflict).exists())
                self.assertEqual(real_run(["git", "status", "--porcelain"], maintenance), before)

    def test_prepare_refuses_existing_directory_and_unstable_tag(self):
        with tempfile.TemporaryDirectory() as temp:
            target = Path(temp)
            marker = target / "keep.txt"
            marker.write_text("keep")
            for tag in ("v4.10.19", "dev", "v4.10.20-rc.1", "../../dev"):
                with self.subTest(tag=tag), self.assertRaises(ValueError):
                    release.prepare(argparse.Namespace(tag=tag, revision=1, destination=target))
            self.assertEqual(marker.read_text(), "keep")

    def test_dependency_fix_applies_only_when_missing(self):
        for before in ("v0.52.0", "v0.56.0", "v0.57.0"):
            with self.subTest(version=before):
                state = {"fixes_checked": False, "dependency_fixes": [
                    {"id": "upstream-pr-2085", "module": "golang.org/x/crypto", "minimum": "v0.56.0"}]}
                needed = before == "v0.52.0"
                infos = [{"Version": before}, {"Version": "v0.56.0" if needed else before}]
                with patch.object(release, "read_state", return_value=state), \
                        patch.object(release, "module_info", side_effect=infos), \
                        patch.object(release, "run") as command, patch.object(release, "write_state"), \
                        patch.object(release, "sha256", return_value="test-module-hash"), \
                        redirect_stdout(io.StringIO()):
                    release.dependencies(argparse.Namespace(source=Path("unused")))
                    self.assertEqual(command.call_count, int(needed))
                    self.assertEqual(state["dependency_fixes"][0]["resolved"], "v0.56.0" if needed else before)
                    self.assertTrue(state["fixes_checked"])

    def test_module_replacement_requires_review(self):
        with patch.object(release, "run", return_value=json.dumps({"Version": "v0.56.0", "Replace": {"Dir": "local"}})):
            with self.assertRaises(ValueError):
                release.module_info(Path("unused"), "golang.org/x/crypto")

    def test_build_refuses_changed_dependencies_before_invoking_make(self):
        state = {"fixes_checked": True, "dependency_files_sha256": {"go.mod": "checked-hash"}}
        with patch.object(release, "read_state", return_value=state), \
                patch.object(release, "run", return_value="") as command, \
                patch.object(release, "sha256", return_value="changed-hash"):
            with self.assertRaises(ValueError):
                release.build(argparse.Namespace(source=Path("unused"), destination=Path("unused-output")))
            self.assertFalse(any(call.args[0][0] == "make" for call in command.call_args_list))


if __name__ == "__main__":
    unittest.main()
