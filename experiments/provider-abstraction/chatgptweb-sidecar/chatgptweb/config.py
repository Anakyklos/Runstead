# ChatGPT Web Sidecar Configuration

from pathlib import Path

from pydantic import Field
from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    # Accounts directory for encrypted cookies
    accounts_dir: Path = Field(
        default_factory=lambda: Path.home() / ".local" / "share" / "runstead" / "chatgptweb",
        validation_alias="CHATGPTWEB_ACCOUNTS_DIR"
    )

    # Master key for cookie encryption (required)
    master_key: str = Field(validation_alias="CHATGPTWEB_MASTER_KEY")

    # Optional proxy
    proxy: str | None = Field(default=None, validation_alias="CHATGPTWEB_PROXY")

    # Default account ID
    default_account: str | None = Field(default=None, validation_alias="CHATGPTWEB_DEFAULT_ACCOUNT")

    # Browser headless mode (false = better bypass)
    headless: bool = Field(default=False, validation_alias="CHATGPTWEB_HEADLESS")

    # Drift detection interval (seconds)
    drift_check_interval: int = Field(default=3600, validation_alias="CHATGPTWEB_DRIFT_INTERVAL")

    # Session warm timeout
    warm_timeout: int = Field(default=60, validation_alias="CHATGPTWEB_WARM_TIMEOUT")

    # Request timeout
    request_timeout: int = Field(default=120, validation_alias="CHATGPTWEB_REQUEST_TIMEOUT")

    model_config = {
        "env_file": ".env",
        "env_file_encoding": "utf-8",
        "case_sensitive": False,
    }


settings = Settings()
