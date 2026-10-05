# Live provider canary HTTP observations

Direct control-request harnesses can use [`sanitized_http.py`](sanitized_http.py)
to emit the same bounded, provider-neutral fields:
`request_count`, `method`, `path`, `http_status`, and `error_type`.

Use fixed origin-form paths (without a query or fragment) and render only the
returned observation. For a successful response, pass its numeric status to
`response_observation`. For urllib failures, branch on the exception class:

```python
from urllib.error import HTTPError, URLError
from pathlib import Path
import sys

# For a harness in a direct child directory of provider-live/.
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sanitized_http import (
    http_error_observation,
    render_observation,
    transport_error_observation,
)

try:
    ...  # one already-authorized direct control request
except HTTPError as exc:
    print(render_observation(
        http_error_observation(request_count, method, path, exc)
    ))
except URLError as exc:
    print(render_observation(
        transport_error_observation(request_count, method, path, exc)
    ))
```

The helper takes `HTTPError.code` only, labels known error classes with fixed
names, and prints `http_status=unknown` when no response status exists. Never
print or serialize the exception, request URL, headers, body, or prompt. This
helper is experiment-harness observability only; it does not classify failure
causes, retry, route requests, or change Runstead runtime authority.

Run its offline synthetic coverage with:

```bash
python3 -m unittest -v test_sanitized_http.py
```
