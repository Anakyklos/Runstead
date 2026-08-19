# Provider Abstraction Research — Issue #16

> **Escopo:** experimento standalone, descartável e research-only. Este diretório não altera as fronteiras de produção do `runstead run`, do governor, do executor ou dos providers.

## Pergunta de pesquisa

O experimento avalia se uma interface mínima de provider pode desacoplar o núcleo do Runstead de conceitos específicos do OmniRoute e, ao mesmo tempo, preservar evidências observáveis de transporte, contabilidade de tentativas, seleção de modelo fail-closed e a distinção entre identidade local e identidade upstream.

O resultado ainda **não é uma decisão de produção**. A pesquisa só pode fornecer evidência para uma futura ADR depois que os caminhos de substituição exigidos forem demonstrados de forma independente e reproduzível.

## Interface Go experimental

A interface draft está em `provider/client.go` e é exercitada pelo fake em `fake/fake.go`. O módulo Go deste diretório é independente do módulo raiz e usa um `replace` local para importar os tipos necessários sem ser atravessado pelos comandos Go executados na raiz.

```go
type ProviderClient interface {
    Complete(ctx context.Context, req ProviderRequest) (ProviderResponse, error)
    HealthCheck(ctx context.Context) (HealthResult, error)
    Models(ctx context.Context) ([]ModelInfo, error)
    Name() string
}
```

A interface mantém `ClientRequestID` como identidade de deduplicação local e `RequestID` como identidade upstream observada pelo transporte. Ela não transforma `err == nil`, HTTP 200 ou uma resposta parcial em prova de entrega.

## Sidecar ChatGPT Web

O sidecar fica em `chatgptweb-sidecar/` e permanece um protótipo experimental. A árvore implementada é deliberadamente pequena:

```text
chatgptweb-sidecar/
├── pyproject.toml
├── chatgptweb/
│   ├── __init__.py
│   ├── __main__.py   # JSON-RPC 2.0 sobre stdio
│   ├── config.py     # perfil de browser e configuração não secreta
│   └── session.py    # warm-up, drift gate, CDP fetch e SSE
├── tests/
│   └── test_sidecar.py
└── README.md
```

As credenciais permanecem dentro do perfil dedicado do browser. O sidecar não extrai, exporta, criptografa nem persiste cookies ou access tokens em arquivos Python. Não existe `CHATGPTWEB_MASTER_KEY`, `crypto.py` ou caminho de fallback para HTTP fora da sessão do browser.

### Fronteira JSON-RPC

O entrypoint aceita `initialize`, `complete`, `cancel`, `health_check`, `models` e `warm_session`. Completions são executadas em tarefas de background para que uma nova linha `cancel` possa ser processada enquanto o streaming está em andamento. O cancelamento recebe o `client_request_id` local e retorna primeiro apenas `state: cancellation_requested`; ele sinaliza o mesmo `asyncio.Event` entregue ao transporte, e só é reportado como cancelamento físico na resposta final quando o mesmo `AbortController` do POST observa `AbortError`.

O transporte segue uma regra de request único. O browser cria um `AbortController`, inicia um único POST e publica um evento `sent` assim que o dispatch físico é confirmado, antes da chegada dos headers. Essa observação já incrementa `send_count`; portanto, timeout, erro ou cancelamento entre o dispatch e os headers não faz a tentativa desaparecer da contabilidade. Timeout e cancelamento nunca abrem um segundo POST.

A conclusão só é declarada quando o SSE termina com `[DONE]`. Se o aborto físico não for observado, ou se o stream terminar sem `[DONE]`, a resposta permanece em estado conservador `timeout_uncertain`. Drift, falha de probe e desafio humano bloqueiam o efeito de modelo sem sobrescrever o baseline persistido.

| Método | Finalidade |
|---|---|
| `initialize` | Inicializa o servidor JSON-RPC. |
| `complete` | Executa uma completion e pode emitir `stream_delta`. |
| `cancel` | Sinaliza uma completion em andamento pelo `client_request_id`. |
| `health_check` | Consulta saúde sem enviar efeito de modelo. |
| `models` | Retorna a lista estática do experimento. |
| `warm_session` | Aquece a sessão e reporta desafios sem resolvê-los automaticamente. |

## Validação e gates

Os testes Python em `chatgptweb-sidecar/tests/` são comportamentais e determinísticos. Eles cobrem reconciliação SSE, isolamento entre completions, preservação do baseline de drift, dispatch pré-headers, aborto físico, timeout incerto, códigos JSON-RPC, serialização de evidência e cancelamento concorrente pelo protocolo stdio.

Os gates locais e de CI são separados por módulo:

```bash
# módulo Go aninhado deste experimento
(cd experiments/provider-abstraction && go test ./... && go vet ./... && go build ./...)

# sidecar Python
(cd experiments/provider-abstraction/chatgptweb-sidecar && \
  python -m pytest tests/ -q && \
  ruff check chatgptweb tests && \
  python -m compileall -q chatgptweb tests)
```

O CI também executa os gates Go da raiz, o parser de protocolo, o spike standalone, o corpus dourado e as verificações de qualidade. Nenhum gate do experimento executa live model turns.

## O que este experimento não altera

O experimento não altera `runstead run`, governor, executor, providers de produção, `LegacyClient`, fake de produção, contabilidade de receipts nem validação de RouteSafety. Também não transforma o sidecar em um provider produtivo nem promete disponibilidade, estabilidade de contrato ou autenticação fora do perfil de browser.

## Próximas evidências

Antes de qualquer decisão de produção, ainda são necessárias evidências independentes de substituição pelo mesmo corpus de protocolo, incluindo taxa de ações válidas, correções, recusas, respostas vazias ou malformadas, recuperação após falha de sessão, latência e precisão diagnóstica. Esta documentação não trata essas medições como já realizadas.

## Referências internas

- ADR de execução durável: `docs/adr/0001-durable-execution.md`
- Interface Go experimental: `provider/client.go`
- Spike standalone de substrate: `../first-party-chatgpt-web-standalone/`
- Issue de pesquisa: `#16`
