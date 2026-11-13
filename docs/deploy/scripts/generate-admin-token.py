#!/usr/bin/env python3
"""
Platform-OPC 生产环境管理员访问令牌 (Admin JWT) 生成工具
用于登录 OPC 管理后台 (/admin/deploy)

用法：
    python3 generate-admin-token.py [PLATFORM_ADMIN_KEY] [有效天数(默认30)]
或者：
    export PLATFORM_ADMIN_KEY="你的秘钥"
    python3 generate-admin-token.py
"""

import os
import sys
import time

try:
    import jwt
except ImportError:
    print("错误: 缺少 pyjwt 库，请先安装: pip install pyjwt")
    sys.exit(1)

def generate_token(secret: str, days: int = 30) -> str:
    now = int(time.time())
    ttl = 86400 * days
    claims = {
        "role": "system_admin",
        "tenant_id": 0,
        "sub": "admin",
        "iat": now,
        "exp": now + ttl
    }
    return jwt.encode(claims, secret.encode("utf-8"), algorithm="HS256")

if __name__ == "__main__":
    secret = sys.argv[1] if len(sys.argv) > 1 else os.getenv("PLATFORM_ADMIN_KEY")
    days = int(sys.argv[2]) if len(sys.argv) > 2 else 30

    if not secret:
        print("错误: 未提供 PLATFORM_ADMIN_KEY！")
        print("请传入参数: python3 generate-admin-token.py <PLATFORM_ADMIN_KEY> [有效天数]")
        print("或设置环境变量: export PLATFORM_ADMIN_KEY=xxx")
        sys.exit(1)

    token = generate_token(secret, days)
    print("=" * 64)
    print(f"Platform-OPC 管理员登录令牌 (有效期: {days} 天):")
    print("=" * 64)
    print(token)
    print("=" * 64)
    print("请使用该令牌在浏览器登录: /admin/login")
