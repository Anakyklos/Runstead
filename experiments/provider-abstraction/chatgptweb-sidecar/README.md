# ChatGPT Web Sidecar — Python + nodriver

Sidecar experimental para o provider abstraction do Runstead. O artefato continua sendo uma **pesquisa não produtiva**, mas seus gates determinísticos agora cobrem a fronteira crítica entre uma chamada de modelo e a evidência observável do transporte.

> O sidecar não transforma uma chamada Web em uma API garantida. Ele só pode declarar sucesso quando a resposta SSE termina com `[DONE]` e mantém estados conservadores quando a entrega não pode ser provada.

## Arquitetura

```text
Runstead Core (Go)                    ChatGPT Web Sidecar (Python)
+------------------+                 Python 3.11+ + nodriver
| Agent Loop       |  JSON-RPC       * Perfil de browser dedicado por conta
| Governor         |  over stdio     * Credenciais permanecem no perfil do browser
| State (SQLite)   |  <----------->  * Um POST físico por completion via page.evaluate
+------------------+                 * AbortController do mesmo POST em timeout/cancelamento
                                     * SSE incremental + reconciliação cumulative/echo
                                     * Detecção de challenge, sem auto-solve
                                     * Drift gate fail-closed antes de cada efeito de modelo
                                     * JSON-RPC 2.0 sobre stdio (`complete` + `cancel`)
```

## Fronteiras de segurança

Cada conta usa um `user_data_dir` próprio. O sidecar não lê, serializa, exporta ou persiste cookies e tokens; o perfil do browser é a fonte de verdade das credenciais. Não há chave mestra, módulo de criptografia de cookies ou arquivo de cookies serializado no caminho de execução.

O request de modelo é iniciado uma única vez dentro do contexto autenticado da página. O Python mantém uma chave opaca para o estado daquele request e pode chamar `AbortController.abort()` apenas no controller associado ao mesmo POST. Timeout, cancelamento explícito e cancelamento do consumidor nunca iniciam uma segunda tentativa. Se o browser não reportar um estado terminal de aborto, o resultado é **não comprovado** e a resposta falha fechada.

## Quick Start

```bash
cd experiments/provider-abstraction/chatgptweb-sidecar
python3 -m venv .venv
source .venv/bin/activate
pip install -e '.[dev]'
python -m chatgptweb
```

## Configuração

As únicas opções de configuração do sidecar são as seguintes:

| Variável | Finalidade | Padrão |
|---|---|---|
| `CHATGPTWEB_ACCOUNTS_DIR` | Diretório-base dos perfis de browser por conta | `~/.local/share/runstead/chatgptweb` |
| `CHATGPTWEB_PROXY` | Proxy opcional para o browser | ausente |
| `CHATGPTWEB_DEFAULT_ACCOUNT` | Conta padrão usada pelo servidor JSON-RPC | ausente |
| `CHATGPTWEB_HEADLESS` | Modo headless do browser | `false` |

Não configure uma chave mestra. A ausência dessa configuração é intencional e não deve impedir o startup.

## JSON-RPC e códigos de erro

O método `complete` recebe `client_request_id`, `model`, `messages` e `stream`. `client_request_id` é uma identidade do chamador; ele não é usado como identidade do request upstream. A identidade upstream, quando disponível na evidência, permanece separada.

O método `cancel` recebe somente `client_request_id` e pode ser enviado enquanto a completion está em andamento. A resposta `state: cancellation_requested` é apenas um ack da solicitação; a confirmação de cancelamento físico só aparece na resposta final da completion depois que o mesmo POST observa `AbortError`. Para uma ID que não está em voo, o método retorna `state: not_in_flight` sem alegar que algum request foi abortado.

| Código | Significado | Condição conservadora |
|---:|---|---|
| `-32001` | autenticação necessária | HTTP 401/403 ou sessão inválida |
| `-32002` | sessão não pronta | challenge, login wall, navegação ou warm-up sem comprovação |
| `-32003` | rate limited | HTTP 429; `retry_after` pode ser preservado como evidência |
| `-32004` | contract drift | falha do drift gate antes ou durante o efeito de modelo |
| `-32005` | falha de transporte | falha determinística comprovada antes de qualquer dispatch, ou erro HTTP 5xx |
| `-32006` | resultado não comprovado | cancelamento, timeout, aborto físico não confirmado, ACK de dispatch perdido, SSE truncado ou erro após envio |

Toda resposta de erro de transporte inclui uma estrutura JSON-serializável de evidência. Ela pode conter `state`, `http_status`, `send_count`, `duration_ms`, `error_code`, `retry_after` e os identificadores upstream fornecidos pelo próprio serviço. Segredos, headers completos, cookies, corpos crus e query strings não são emitidos no protocolo.

## Estados de transporte

A máquina de estados não deriva entrega de `err == nil` nem de HTTP 200 isolado. O caminho nominal é `no_send_observed → send_observed → response_started → completed`. O encerramento de uma resposta só é sucesso quando o marcador SSE `[DONE]` foi observado.

| Estado | Interpretação |
|---|---|
| `no_send_observed` | nenhum evento de transporte foi observado |
| `send_observed` | o browser confirmou o dispatch do POST; headers podem ainda não ter chegado |
| `response_started` | o corpo da resposta começou a ser observado |
| `completed` | `[DONE]` foi observado e a resposta é declarada completa |
| `transport_failed` | falha determinística sem evidência de entrega do efeito |
| `timeout_uncertain` | o efeito pode ter sido aceito; a entrega não é comprovável |
| `canceled_pre_dispatch` | cancelamento observado antes de qualquer dispatch; `send_count=0` e nenhum POST é alegado |
| `canceled` | o mesmo POST reportou `AbortError` após `abort()` físico |

## Drift gate

Antes de cada envio de efeito de modelo, o sidecar calcula o hash SHA-256 do recurso Sentinel observado e compara com a baseline persistida por conta. Uma alteração ou uma falha na sondagem gera `DriftDetected` e bloqueia o envio. A baseline nunca é sobrescrita quando o valor atual diverge; isso evita transformar drift em uma nova normalidade silenciosa.

## SSE e cancelamento físico

O `SSEReconciler` trata o conteúdo recebido como cumulativo e emite somente o sufixo novo, suprimindo duplicidades e evitando slicing negativo. Cada completion cria seu próprio reconciliador, portanto chamadas concorrentes não compartilham estado de conteúdo.

O transporte usa um único `fetch` físico no browser, com `ReadableStream` e um `AbortController` associado àquele request. Em timeout ou cancelamento pós-dispatch, o sidecar chama `abort()` no mesmo controller e aguarda o estado terminal do mesmo fetch. Cancelamento antes do dispatch é reportado como `canceled_pre_dispatch`, com `send_count=0`; somente o estado `canceled` pode alegar que o POST observou `AbortError`. Se o script de start executar o fetch, mas o ACK `{started: true}` se perder na fronteira CDP, o efeito é tratado como `timeout_uncertain` e `send_count` não volta para zero.

## Testes e gates

A suíte determinística não usa credenciais, não abre browser e não executa live model turns. Ela cobre reconciliação SSE sequencial e concorrente, baseline de drift persistente, falha do probe, dispatch pré-headers, cancelamento pré-dispatch, aborto físico pós-dispatch, perda do ACK de start, aborto não comprovado, mapeamento 401/403/429/5xx, truncamento sem `[DONE]`, separação entre erro antes e depois do dispatch, cleanup de IDs após erro de preflight, serialização da evidência, códigos JSON-RPC, cancelamento concorrente pelo stdio, identidade dos requests e warm idempotente.

```bash
cd experiments/provider-abstraction/chatgptweb-sidecar
python3 -m pytest tests/ -q
ruff check chatgptweb tests
python3 -m compileall -q chatgptweb tests
```

## Limitações declaradas

A confiabilidade do handshake Sentinel/Turnstile e a durabilidade do comportamento sob mudanças reais do ChatGPT Web continuam sem comprovação live. O sidecar detecta challenges, mas não resolve CAPTCHA, Turnstile ou login wall. O experimento também não promete estabilidade de endpoint, contrato de modelo ou disponibilidade do serviço upstream.

## Referências de implementação

A implementação deve ser lida junto de `docs/adr/0001-durable-execution.md`, do README do experimento em `experiments/provider-abstraction/` e da própria evidência determinística no diretório `tests/`. O artefato não executa merge automático nem live model turns como parte de seus gates.
