# Issue #84 — substrate bake-off: Playwright/Chromium vs. chromedp/CDP

**Status:** pesquisa isolada concluída; nenhuma integração de produção; readiness pass local executada com zero model turns; canary real bloqueada antes de uma sessão autenticada. **Recomendação para revisão do mantenedor:** manter Playwright + Chromium como candidato do canary assistido, sem tratar a escolha como decisão arquitetural aprovada.

## Resumo executivo

O bake-off comparou os dois finalistas exigidos pela Issue #84 contra a mesma fixture HTTP/SSE sintética, com Chromium real, perfis persistentes sintéticos e contagem física independente no servidor. Na rerun oficial, ambos passaram os gates autochecking: um POST físico para cada submit normal, exatamente dois POSTs no redirect (`/submit` -> `/effect-final`), exatamente um POST no `fetch-correlation`, Service Worker controlado e observado, deadline real separado de cancelamento explícito, cancelamento após response-start, crash/disconnect sem retry, classificação conservadora após possibilidade de dispatch, isolamento de dois perfis e cleanup sem processos remanescentes.[1] [2]

A recomendação provisória é **Playwright + Chromium**. A vantagem não é “Node ser melhor”: Playwright exigiu aproximadamente **413 linhas** de runner/benchmark JavaScript, contra **956 linhas** no runner chromedp, oferece lifecycle de contexto persistente mais curto e deixou a matriz funcional comparável com menos código mantido pelo Runstead. O chromedp foi mais rápido no bootstrap frio medido e permaneceu Go-native, mas exigiu mais lifecycle explícito, correlação Network/Fetch, encerramento de árvore de processos e alinhamento de schema CDP. A primeira versão compatível usada no experimento apresentou erros de unmarshalling em `IPAddressSpace=Loopback` com o Chromium disponível; a lane só ficou limpa após migrar para chromedp `v0.16.0`/cdproto de julho de 2026. Isso é evidência de custo real de acoplamento navegador–schema, não motivo para declarar chromedp inviável.

A recomendação **não autoriza** abrir perfil real, usar conta, abrir ChatGPT Web ou executar model turn. Os claims de upstream acceptance, comportamento de sessão autenticada e estabilidade do canary permanecem não provados e dependem de revisão explícita do mantenedor.[1]

## Metodologia e fronteira

A pesquisa foi executada na branch `research/issue-84-substrate-bakeoff`, partindo do commit `e24cb45afa576c5a016bc768fc934355f02a9915` da `main`. A fixture local em `experiments/substrate-bakeoff/fixture` registra cada request físico com método, rota, status, redirect, Service Worker, bytes e identificador sanitizado. Os dois candidatos executam as mesmas rotas, atrasos e falhas. Não houve retry, fallback, rotação de conta, CAPTCHA/MFA bypass, provider wiring, mudança de governor, alteração de durable task state ou execução real contra ChatGPT.[1] [3]

A matriz funcional contém **13 cenários Playwright** e **14 cenários chromedp**. O caso adicional do chromedp é `fetch-correlation`, criado para demonstrar de forma explícita a relação entre `Network.RequestID`, `Fetch.RequestID`, `networkId` e `redirectedRequestId`; Playwright não precisa dessa lane CDP específica porque sua superfície de request/response já é observada diretamente. Todos os outros cenários são comuns.

### Ambiente reproduzível

| Item | Valor observado |
| --- | --- |
| Sistema | Ubuntu 24.04.4 LTS, amd64 |
| Chromium | `/usr/bin/chromium`, `151.0.7922.71` |
| Playwright | `1.62.1` |
| Node | `v22.13.0` |
| Go do experimento | `1.26.1` |
| chromedp | `v0.16.0` |
| cdproto | `v0.0.0-20260714215040-dc233986426f` |
| Fixture | `127.0.0.1:18765`, servidor Go local |
| Perfil | diretórios sintéticos sob `experiments/substrate-bakeoff/profiles/` |

Os artefatos principais são `experiments/substrate-bakeoff/output/playwright-results.json`, `experiments/substrate-bakeoff/output/chromedp-results.json`, `experiments/substrate-bakeoff/output/overhead-playwright.json` e `experiments/substrate-bakeoff/output/metrics-summary.txt`. O benchmark chromedp é incorporado no artefato principal para manter o lifecycle medido no mesmo runner. Cada artefato funcional contém `gate_failures`; uma lista não vazia faz o runner retornar código diferente de zero.

## Vocabulário de evidência

Os runners distinguem observação de browser/fixture de aceitação upstream. `dispatch_observed` significa que o browser criou/observou o request; `response_started` significa que a fixture/browser registrou início de resposta; `completed` exige terminal definido na fixture; `sent_unconfirmed` é o estado conservador quando houve possibilidade de dispatch sem prova de aceitação; `not_sent` só é usado no cancelamento antes da criação física; `physical_abort_unproven` impede transformar abort, timeout, crash ou disconnect em prova otimista de não envio; `unknown_submission` é a categoria de recuperação reservada para efeito possivelmente enviado cuja aceitação não foi estabelecida.

> Se houve possibilidade de dispatch, a matriz não classifica o caso como `not_sent` por causa de `AbortError`, `net::ERR_ABORTED`, timeout, cancelamento ou morte do processo. Essa regra segue o princípio de classificação conservadora da Issue #84 e da arquitetura do Runstead.[1] [3]

## Physical-send accounting

| Cenário | Playwright | chromedp | Leitura conservadora |
| --- | --- | --- | --- |
| `normal` | 1 POST físico; `duplicate_gate=pass_exactly_one_effect`; `sent_unconfirmed` | 1 POST físico; `duplicate_gate=pass_exactly_one_effect`; `sent_unconfirmed` | Um submit lógico não virou múltiplos efeitos. |
| `redirect` | 2 hops físicos; `duplicate_gate=pass_redirect_exact_sequence` | 2 hops físicos; `duplicate_gate=pass_redirect_exact_sequence`; mapeamento Fetch/Network observado | A sequência física exigida é exatamente `/submit` -> `/effect-final`; qualquer hop extra falha. |
| `service-worker` | 1 POST físico e `service_worker_controlled=true` | 1 POST físico e `service_worker_controlled=true` | O tráfego sob SW continuou observável. |
| `cancel-before-dispatch` | 0 POST; `not_sent` | 0 POST; `not_sent` | Único caso em que a fixture comprovou ausência de criação física. |
| Cancelamento/timeout após possibilidade de dispatch | exatamente 1 POST; `sent_unconfirmed` | exatamente 1 POST; `sent_unconfirmed` | Abort local não prova ausência de efeito upstream; zero POST também falha o contrato quando o dispatch era esperado. |

O `duplicate_gate` exige a sequência exata de dois POSTs no redirect, exatamente um POST em `fetch-correlation`, zero POST em `cancel-before-dispatch` e exatamente um POST em todos os demais cenários que esperam dispatch. Nenhum runner implementa retry ou fallback para descobrir o destino do primeiro request. Os gates de contrato também exigem dispatch, fase/terminal, estado conservador, ordenação de cancelamento, crash/disconnect, Service Worker e cleanup. São autoridade de aceitação: se qualquer propriedade obrigatória falhar, `npm test`, o binário Go e `run.sh` retornam código diferente de zero; o JSON é evidência complementar.

## Matriz funcional completa

| Cenário | Fixture e evento exercitado | Playwright | chromedp | Estado/observação comum |
| --- | --- | --- | --- | --- |
| Normal | POST comum | passou | passou | 1 POST físico; dispatch observado. |
| Redirect | POST 307/POST final | passou | passou | 2 hops físicos; chromedp comprovou mapeamento Network/Fetch. |
| Service Worker | registro, `ready`, reload e fetch controlado | passou | passou | controle real e contagem SW positiva. |
| Timeout antes dos headers | headers atrasados | passou | passou | request criado; `sent_unconfirmed`; abort não vira `not_sent`. |
| Cancel após headers | body atrasado | passou | passou | response iniciou; cancelamento separado de abort físico. |
| Cancel in-flight | resposta mantida aberta | passou | passou | dispatch possível; estado conservador. |
| SSE completo | terminal esperado | passou | passou | `completed`; sem segundo request para ler body. |
| SSE truncado | stream termina sem terminal | passou | passou | incompleto/incerto; não é sucesso. |
| EOF sem terminal | EOF sem marcador terminal | passou | passou | incompleto/incerto; não é sucesso. |
| Resposta parcial | body parcial | passou | passou | incompleto/incerto; não é sucesso. |
| Cancel antes do dispatch | cancelamento antes da criação física | passou | passou | 0 POST; `not_sent` comprovado pela fixture. |
| Browser kill in-flight | mata Chromium durante request | passou | passou | possível efeito preservado como `sent_unconfirmed`; sem retry. |
| Controller disconnect in-flight | desconexão abrupta do transporte Playwright, mantendo o browser independente | passou | passou | perda inesperada do controller/helper observada; sem retry e com cleanup. |
| Fetch correlation | `Fetch.requestPaused`, `networkId`, redirect mapping | não aplicável como lane CDP dedicada | passou | relação explícita registrada no artefato chromedp. |

Em ambos os braços, headers e body foram associados ao mesmo identificador observado, e a leitura de stream não emitiu um segundo submit. A fixture usa SSE apenas para exercitar lifecycle e terminal; não há semântica específica de ChatGPT no substrato.

## Lifecycle, crash e perfis

Os dois runners criaram perfis persistentes sintéticos, reabriram o mesmo perfil e verificaram que dois perfis diferentes não compartilharam o marcador de `localStorage`. O gate `isolated=true` passou nos dois artefatos. Os metadados emitidos usam apenas `profile_ref` e strings sintéticas; não há cookies, tokens, passwords, conteúdo privado ou caminho pessoal exportado.

O cenário de browser kill encerra o processo durante request e inspeciona a árvore posterior. O cenário de controller disconnect mata abruptamente o transporte/controller helper enquanto o browser continua sendo uma responsabilidade independente e a perda de conexão é observada; isso permanece distinto de matar o browser. Nenhum cenário reenvia o submit para “descobrir” se o primeiro funcionou. O resultado é intencionalmente conservador: quando o request pode ter sido dispatchado, o estado permanece possivelmente enviado e não confirmado.

### Responsabilidades de lifecycle

| Responsabilidade | Playwright | chromedp |
| --- | --- | --- |
| Browser discovery/download | Playwright pode gerenciar browser em instalação padrão; esta lane fixa `/usr/bin/chromium` e não baixa em cada run. | Nenhum downloader; o runner precisa localizar/fixar `/usr/bin/chromium`. |
| Process allocation | `launchPersistentContext` encapsula criação e fechamento do browser/contexto. | `exec.Command`, porta CDP, polling do endpoint, allocator remoto e kill de árvore são responsabilidade do proof. |
| Profile path | Contexto persistente recebe path explícito. | `UserDataDir` é passado ao processo Chromium e o runner precisa cuidar de locks/cleanup. |
| Target/page lifecycle | Contexto e página são objetos de alto nível; fechamento é direto. | `NewRemoteAllocator`, `NewContext`, cancelamento de target e allocator precisam ser ordenados. |
| Event correlation | Eventos de request/response são expostos pela página. | `Network.RequestID` e `Fetch.RequestID` precisam ser correlacionados; redirects exigem `networkId`/`redirectedRequestId`. |
| Interception | Não foi necessária para o gate comum; redirect foi observado pelos eventos de página. | `fetch.Enable` e `requestPaused` foram usados na lane de correlação. |
| Streaming | Leitura de body/terminal implementada sobre a página e fixture. | Leitura de body/terminal implementada sobre eventos CDP e fixture. |
| Cancellation | `AbortController`/fechamento de contexto e browser kill exercitados. | cancelamento de target/allocator, cancelamento de action e kill de processo exercitados. |
| Shutdown | `context.close()` e inspeção da árvore. | kill idempotente, `sync.Once`, encerramento da árvore e inspeção da árvore. |
| Crash detection | morte do contexto/browser observada pelo runner. | processo Chromium e desconexão CDP observados diretamente. |
| Profile locking | Delegado ao contexto/browser, com perfis sintéticos sequenciais. | UserDataDir e encerramento de árvore pertencem ao runner; concorrência insegura não foi habilitada. |
| Browser compatibility | Playwright 1.62.1 usado com Chromium fixado. | chromedp 0.16.0/cdproto de 2026 necessário para o schema do Chromium 151. |
| Runtime externo | Node 22 + pacote Playwright/driver. | Nenhum Node; Go 1.26.1 + módulos Go. |
| Runstead-owned code | 372 linhas de runner + 41 de benchmark, além de package/lockfile. | 956 linhas de runner, incluindo lifecycle, CDP, métricas, contratos e cleanup. |

## Custo medido

Foram feitas três execuções frias por candidato. Startup mede o intervalo até o browser/endpoint estar disponível; navigation mede navegação até o elemento da fixture estar visível; shutdown mede o fechamento/kill; RSS é a soma dos processos Chromium observados após navegação. São medidas locais, não uma previsão de produção.

| Candidato | Startup médio | Navegação média | Shutdown médio | RSS médio | Processos observados |
| --- | ---: | ---: | ---: | ---: | ---: |
| Playwright | 244,190 ms | 40,687 ms | 55,993 ms | 872.192 KB | 9 |
| chromedp | 173,709 ms | 98,129 ms | 44,207 ms | 862.480 KB | 9 |

O chromedp foi aproximadamente 70 ms mais rápido no bootstrap frio e 12 ms mais rápido no shutdown; Playwright foi aproximadamente 57 ms mais rápido na navegação medida. O RSS e a quantidade de processos foram essencialmente equivalentes dentro desta execução. A árvore Playwright foi `Node -> Playwright driver -> Chromium`; a árvore chromedp foi `Go runner -> chromedp -> Chromium CDP websocket`.

### Packaging e dependências

| Candidato | Evidência de packaging |
| --- | --- |
| Playwright | `node_modules` local de aproximadamente 19 MB; um pacote direto (`playwright@1.62.1`), Node 22 e driver Playwright; lockfile dedicado; browser fixado do sistema nesta lane. |
| chromedp | módulo Go isolado de aproximadamente 36 KB no checkout, 8 módulos transitivos listados além do módulo principal; Go 1.26.1; nenhum pacote Node/browser downloader; forte dependência do schema CDP do Chromium. |
| Fixture | aproximadamente 16 KB e 227 linhas; módulo Go separado; compartilhada pelos dois finalistas. |
| Proof total | 1.733 linhas nos arquivos de fixture/runners/benchmark/scripts/README, sem código de produção. |

A instalação Playwright tem uma superfície externa maior; o chromedp tem uma superfície de lifecycle e compatibilidade maior dentro do código mantido pelo Runstead. A rerun final gerou três amostras frias por candidato e `gate_failures=[]` nos dois artefatos funcionais. Nenhuma dessas métricas isoladamente escolhe o vencedor.

## Compatibilidade e atualização

A primeira versão chromedp fixada durante a pesquisa emitia erros de unmarshalling para `IPAddressSpace=Loopback`, valor emitido pelo Chromium 151 e não reconhecido pelo cdproto antigo. O experimento não mascarou esses erros: atualizou para `chromedp v0.16.0`, `cdproto v0.0.0-20260714215040-dc233986426f` e Go 1.26.1, após o que a matriz terminou sem o warning. Isso demonstra que a lane Go-native ainda exige uma política explícita de sincronização browser–cdproto.

Playwright também precisa de sincronização entre pacote, driver e browser, mas a superfície de lifecycle de contexto, página, perfil e eventos permanece menor. O custo de atualização/rollback de ambos precisa ser tratado como pacote experimental versionado; não foi adicionado download pesado ao CI Go normal.

## Recomendação e kill criteria

Recomendo **Playwright + Chromium** para um canary futuro, se e somente se o mantenedor autorizar o canary na mesma PR após revisão. A justificativa é a combinação de menor código Runstead-owned, lifecycle de alto nível, evidência de perfis/Service Worker/cancelamento suficiente na fixture e ausência de erros de schema na versão fixada. O ganho de startup do chromedp é real, mas não compensa sozinho o lifecycle adicional e a sensibilidade de versão observada.

A recomendação deve ser revertida e a escolha suspensa se qualquer uma destas condições ocorrer no canary autorizado: múltiplos POSTs físicos para um submit lógico sem redirect explicitamente contabilizado; impossibilidade de distinguir dispatch de não envio; Service Worker esconder tráfego sem lane bloqueada e documentada; crash/disconnect gerar resubmit; perfil real vazar segredo; incompatibilidade de Chromium/CDP não puder ser fixada; ou cleanup deixar processos órfãos. Se ambos os finalistas falharem esses gates, nenhum deve ser conectado à produção.

## Claims ainda não provados

A pesquisa não prova aceitação upstream depois do dispatch, idempotência real do endpoint, comportamento de autenticação, persistência de sessão real, corrupção/upgrade de um perfil real, concorrência segura entre processos de produção, performance sob carga, disponibilidade em máquinas sem Chromium compatível ou o resultado de um model turn. Também não prova que o canary Playwright passará em uma conta autorizada; apenas recomenda qual candidato testar primeiro.

O cenário de Service Worker é uma prova sintética local de controle e observabilidade. O cenário de controller disconnect comprova cleanup e classificação no helper deste proof, não a recuperação completa de um processo de produção. O RSS é uma amostra local e não deve ser usado como orçamento universal.

## Readiness pass do canary autorizado

Em 2026-08-19, foi executada a suíte determinística e o modo `dry` de `experiments/first-party-chatgpt-web-standalone/run_spike.mjs`, usando Node `v22.13.0`, Chromium `151.0.7922.71`, CDP via `DevToolsActivePort + /json/version` e o perfil descartável `profiles/standalone-spike`. O harness chegou à página, mas classificou a prontidão como `login_required`; como não havia uma sessão autenticada autorizada disponível, a execução parou antes do runtime autenticado. O registro sanitizado está em `experiments/first-party-chatgpt-web-standalone/evidence/issue-84-canary-readiness.json`.[6]

| Evidência da readiness pass | Resultado |
| --- | --- |
| Model turns emitidos | `0` |
| POST físico de efeito de modelo | `0` |
| Identidade de rota e request | Não observada; parada anterior ao dispatch |
| Atribuição de resposta/completion | Não aplicável |
| Retry, resubmit ou fallback | Não executado |
| Estado final | `blocked_before_dispatch` |
| Classificação | `fail_closed` |

Essa execução confirma apenas que o caminho sem sessão não avança para um efeito de modelo. Ela **não** prova login, estabilidade de sessão, aceitação upstream, atribuição de response/completion nem comportamento de um model turn real.

## Gate do canary real

O canary real permanece **bloqueado**. Não foram usados conta autenticada, cookies, tokens, perfil autorizado, model turn ou exportação de credenciais. A alternativa segura disponível neste ambiente é apenas repetir a readiness pass e a matriz sintética; executar `live` sem um perfil autorizado violaria o escopo da revisão. O PR deve continuar em draft até que o mantenedor forneça ou autorize um perfil dedicado por um canal aprovado. Se e quando isso ocorrer, o canary deve permanecer na mesma PR, com escopo mínimo, sem retry automático e sem tornar `sent_unconfirmed` otimisticamente em `not_sent`.

## Validações executadas

A fixture e os dois runners foram executados com Chromium real na rerun oficial limpa. Também foram executados os testes do módulo chromedp, `go vet` do módulo experimental, `npm test` Playwright, benchmark dos dois braços, gates de contrato por cenário, redirect, Service Worker, cancellation, browser kill, controller disconnect e profile isolation. Os dois artefatos funcionais registraram `gate_failures=[]`; qualquer falha teria encerrado a lane com código diferente de zero. O script `experiments/substrate-bakeoff/run.sh` reproduz a sequência.

As validações de produção do repositório — `gofmt -l .`, `go test ./...`, `go vet ./...`, `go build ./cmd/runstead`, `go test -race ./...`, `bash experiments/protocol/test.sh` e `git diff --check` — foram executadas antes da atualização da PR e permanecem separadas do bake-off. A pesquisa não altera o `go.mod` principal nem o wiring de produção.

## Referências

[1]: https://github.com/pedro-labsabs/Runstead/issues/84 "Runstead Issue #84 — Run the #16 substrate bake-off"
[2]: https://github.com/pedro-labsabs/Runstead/issues/16 "Runstead Issue #16 — substrate and provider decisions"
[3]: https://github.com/pedro-labsabs/Runstead/blob/main/docs/architecture.md "Runstead architecture"
[4]: https://github.com/pedro-labsabs/Runstead/issues/49 "Runstead Issue #49 — security/provenance constraints"
[5]: https://github.com/pedro-labsabs/Runstead/issues/74 "Runstead Issue #74 — drift/reconciliation constraints"
[6]: ../../experiments/first-party-chatgpt-web-standalone/evidence/issue-84-canary-readiness.json "Sanitized canary readiness record — 2026-08-19"
