"""核对 SQLite 中的凭据存储形态。

用途：验证「外部凭据加密保存」这条硬性要求是否真的落地——
数据库里不应出现 Dashboard 密码明文，而应是 enc:v1: 前缀的密文。

用法：python check-db.py <db 路径> <不应出现的明文密码>
"""

import sqlite3
import sys


def main() -> int:
    if len(sys.argv) < 3:
        print("用法: python check-db.py <db 路径> <明文密码>")
        return 2

    db_path, plain = sys.argv[1], sys.argv[2]

    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        cur = con.cursor()

        print("=== 表清单 ===")
        for (name,) in cur.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
        ):
            print("  " + name)

        print("\n=== 迁移记录 ===")
        for row in cur.execute("SELECT version, name FROM schema_migration ORDER BY version"):
            print(f"  v{row[0]}  {row[1]}")

        print("\n=== app_user（口令必须只有哈希）===")
        for row in cur.execute("SELECT id, username, password_hash FROM app_user"):
            h = row[2]
            kind = "bcrypt" if h.startswith("$2") else "!! 非 bcrypt !!"
            print(f"  id={row[0]}  username={row[1]}  hash({kind})={h[:32]}...")
            if plain and plain in h:
                print("  !! 口令哈希中出现明文 !!")

        print("\n=== session（只存 token 哈希）===")
        n = cur.execute("SELECT COUNT(1) FROM session").fetchone()[0]
        print(f"  会话数={n}")

        print("\n=== node ===")
        leaks = 0
        for row in cur.execute(
            "SELECT id, name, api_url, username, password_enc, timeout_seconds, enabled FROM node"
        ):
            enc = row[4] or ""
            status = "已加密" if enc.startswith("enc:v1:") else ("空" if not enc else "!! 未加密 !!")
            print(f"  id={row[0]}  name={row[1]}  api_url={row[2]}  username={row[3]}")
            print(f"    password_enc = [{status}] {enc[:56]}...")
            print(f"    timeout={row[5]}s  enabled={row[6]}")
            if plain and plain in enc:
                leaks += 1
                print("    !! 密文字段中出现明文 !!")

        print("\n=== 整库扫描明文 ===")
        hits = []
        for (name,) in cur.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
        ):
            cols = [c[1] for c in cur.execute(f'PRAGMA table_info("{name}")')]
            for row in cur.execute(f'SELECT * FROM "{name}"'):
                for col, val in zip(cols, row):
                    if isinstance(val, str) and plain and plain in val:
                        hits.append(f"  {name}.{col} = {val[:80]}")
                    if isinstance(val, bytes) and plain.encode() in val:
                        hits.append(f"  {name}.{col} (blob) 含明文")
        if hits:
            leaks += len(hits)
            print("!! 发现明文：")
            for h in hits:
                print(h)
        else:
            print("  全库无明文密码")

        print("\n=== 审计（确认不含凭据）===")
        for row in cur.execute(
            "SELECT at, actor, action, target, result, detail FROM audit ORDER BY id"
        ):
            print(f"  {row[0][:19]}  {row[1]:<12} {row[2]:<14} {row[3]:<28} {row[4]:<12} {row[5][:60]}")

        print("\n=== 结论 ===")
        print("  凭据加密: " + ("FAIL 发现明文" if leaks else "PASS"))
        return 1 if leaks else 0
    finally:
        con.close()


if __name__ == "__main__":
    sys.exit(main())
