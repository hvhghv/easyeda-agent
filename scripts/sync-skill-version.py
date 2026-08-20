#!/usr/bin/env python3
"""把 skills/easyeda-agent/SKILL.md 的 metadata.version 同步到发版版本号。

为什么需要它:Agent Skills 规范允许 frontmatter 带 `metadata`(string→string),
我们在里面记 version,让 ClawHub / 目录索引 / 离线拿到 skill 包的人都能看出这份
skill 对应哪个 CLI。但 `make release` 只把版本传给 clawhub 命令行、并 bump
extension.json,**不碰 SKILL.md** —— 不同步就会漂移(skill 里写着 1.0.2,实际
发的是 1.1.0)。所以 release 在 bump 连接器的同一步里调用本脚本。

CLAUDE.md 的「版本号约定」:CLI / connector / skill 始终同一个版本号。

用法:
    python3 scripts/sync-skill-version.py 1.0.3        # 写入
    python3 scripts/sync-skill-version.py 1.0.3 --check  # 只校验,不一致则非零退出
"""

import argparse
import pathlib
import re
import sys

SKILL = pathlib.Path(__file__).resolve().parent.parent / "skills" / "easyeda-agent" / "SKILL.md"
# 只匹配 frontmatter 里 metadata 块下的 version 行(两空格缩进),不会误伤正文。
PATTERN = re.compile(r'(?m)^(  version:\s*)"([^"]*)"$')


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("version", help="版本号,不带 v 前缀(如 1.0.3);带了也会被剥掉")
    ap.add_argument("--check", action="store_true", help="只校验是否已同步,不写入")
    args = ap.parse_args()

    want = args.version.lstrip("v")
    if not SKILL.is_file():
        print(f"error: {SKILL} 不存在", file=sys.stderr)
        return 1

    text = SKILL.read_text(encoding="utf-8")
    match = PATTERN.search(text)
    if not match:
        print(f"error: {SKILL} 的 frontmatter 里找不到 metadata.version —— "
              "是不是 frontmatter 被改过?", file=sys.stderr)
        return 1

    have = match.group(2)
    if have == want:
        print(f"  skill version 已是 {want}")
        return 0

    if args.check:
        print(f"error: skill version 是 {have},期望 {want} —— 跑 "
              f"`python3 scripts/sync-skill-version.py {want}` 同步", file=sys.stderr)
        return 1

    SKILL.write_text(PATTERN.sub(lambda m: f'{m.group(1)}"{want}"', text, count=1), encoding="utf-8")
    print(f"  skill version {have} → {want}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
