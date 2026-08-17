# Encryption utilities for credential storage

import base64

from cryptography.fernet import Fernet
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC

from chatgptweb.config import settings


def get_encryption_key() -> bytes:
    """Derive encryption key from master key + machine ID."""
    machine_id = ""
    try:
        with open("/etc/machine-id") as f:
            machine_id = f.read().strip()
    except FileNotFoundError:
        machine_id = "unknown-machine"

    kdf = PBKDF2HMAC(
        algorithm=hashes.SHA256(),
        length=32,
        salt=machine_id.encode(),
        iterations=100000,
    )
    key = base64.urlsafe_b64encode(kdf.derive(settings.master_key.encode()))
    return key


def encrypt_cookies(cookies: dict) -> bytes:
    """Encrypt cookies dictionary to bytes."""
    f = Fernet(get_encryption_key())
    import json
    return f.encrypt(json.dumps(cookies).encode())


def decrypt_cookies(data: bytes) -> dict:
    """Decrypt bytes to cookies dictionary."""
    f = Fernet(get_encryption_key())
    import json
    return json.loads(f.decrypt(data).decode())
