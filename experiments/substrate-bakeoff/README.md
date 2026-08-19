# Issue #84 — substrate bake-off

Este diretório contém uma pesquisa isolada para comparar **Playwright 1.62.1 + Chromium 151** com **chromedp 0.16.0/cdproto + Chromium 151** contra a mesma fixture HTTP/SSE local. A pesquisa não altera `runstead run`, provider default, governor, estado durável, agent loop ou qualquer provider de produção. A fronteira segue a Issue #84 e as decisões de #16: o navegador é descartável, a fixture é sintética e nenhum segredo sai de um perfil experimental.[1] [2]

## Estrutura

| Área | Conteúdo |
| --- | --- |
| `fixture/` | Servidor Go comum com POST, redirect, Service Worker, SSE, atrasos, cancelamento, crash/disconnect e contagem física. |
| `playwright/` | Proof Node/Playwright com contexto persistente, eventos de página, cenários e benchmark de overhead. |
| `chromedp/` | Módulo Go isolado com chromedp/cdproto, CDP Network/Fetch, allocator remoto, perfis e matriz equivalente. |
| `output/` | Artefatos JSON sanitizados gerados localmente; não contêm cookies, tokens, passwords ou conteúdo privado. |
| `run.sh` | Execução reprodutível da fixture, dos dois braços, dos testes e dos benchmarks. |

## Execução

O ambiente de referência usou Chromium `151.0.7922.71`, Node `v22.13.0`, Playwright `1.62.1`, Go `1.26.1`, chromedp `v0.16.0` e cdproto `v0.0.0-20260714215040-dc233986426f`. O script não assume caminhos locais: detecta `go`/Chromium no `PATH` e permite sobrescrever tudo por ambiente.

Para repetir a matriz com a fixture local:

```bash
./run.sh
```

Inputs opcionais:

```bash
GO_BIN=/caminho/para/go \
CHROMIUM_PATH=/caminho/para/chromium \
RUNSTEAD_FIXTURE_ADDR=127.0.0.1:18765 \
./run.sh
```

Para usar uma fixture já em execução, sem iniciar a fixture local:

```bash
START_FIXTURE=0 RUNSTEAD_FIXTURE_URL=http://127.0.0.1:18765 ./run.sh
```

O script usa `npm ci` a partir do `package-lock.json`, não executa `go mod tidy` e retorna código não-zero se qualquer gate autochecking falhar. Os contratos exigem exatamente um POST físico nos cenários que esperam dispatch, zero apenas em `cancel-before-dispatch`, e exatamente dois hops (`/submit` → `/effect-final`) no redirect. Também exigem dispatch, fase/terminal, estado conservador, deadline/cancelamento ordenados, cleanup, isolamento de perfil, controle/observação do Service Worker, correlação Fetch/Network e disconnect abrupto do transporte. `gate_failures` é calculado antes da gravação de cada artefato; qualquer lista não vazia falha a execução.

Para os checks separados, use:

```bash
(cd fixture && go test ./...)
(cd playwright && npm ci --ignore-scripts && npm test)
(cd chromedp && "$GO_BIN" test ./...)
(cd chromedp && "$GO_BIN" vet ./...)
```

O script usa um Chromium já instalado e, portanto, não baixa um browser a cada execução. A lane Playwright registra essa escolha e a dependência do runtime Node/driver; a lane chromedp registra a dependência do toolchain Go e do schema CDP observado. A instalação de browser, atualização, rollback e sincronização de versões permanecem parte do custo operacional descrito em `docs/research/issue-84-substrate-bakeoff.md`.

## Segurança e escopo

Todos os perfis são diretórios sintéticos criados sob `profiles/`. O único marcador persistido é uma string de teste em `localStorage`, usada para provar reuso e isolamento. Os artefatos expõem apenas `profile_ref`, eventos sanitizados, hashes de corpo, contagens, status e caminhos relativos; não exportam cookies, tokens, passwords, headers de autenticação ou caminhos pessoais.

A matriz não executa ChatGPT Web, não abre perfil autenticado, não faz model turn real, não resolve CAPTCHA/MFA, não cria retry/fallback e não inicia nenhum efeito upstream. O canary real está deliberadamente bloqueado até revisão do mantenedor, como exigido pela tarefa.[1]

## Referências

[1]: https://github.com/pedro-labsabs/Runstead/issues/84 "Runstead Issue #84 — substrate bake-off"
[2]: https://github.com/pedro-labsabs/Runstead/issues/16 "Runstead Issue #16 — provider abstraction and browser substrate decisions"
