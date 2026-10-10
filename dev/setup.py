"""Generate local-only Compose credentials; never overwrite existing credentials."""
import os
from pathlib import Path
import secrets

path = Path(__file__).resolve().parents[1] / ".env"
keys = ("MYSQL_PASSWORD", "MYSQL_ROOT_PASSWORD",
        "NAKAMA_SERVER_KEY", "NAKAMA_SESSION_KEY", "NAKAMA_REFRESH_KEY",
        "NAKAMA_CONSOLE_PASSWORD", "NAKAMA_CONSOLE_KEY", "NAKAMA_HTTP_KEY")
try:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    print("Existing .env preserved.")
else:
    with os.fdopen(fd, "w") as output:
        output.write("# Generated local-only credentials. Do not commit or use in production.\n")
        for key in keys:
            output.write(f"{key}={secrets.token_hex(24)}\n")
    print("Created .env with local credentials (mode 0600).")
