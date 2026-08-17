# Engenharia reversa — Pool de contas e fallback do OmniRoute (camada acima do executor)

**Projeto-alvo:** Runstead
**Referência examinada:** OmniRoute `main` @ `aa5b77e` (`docs: add OmniCopilot … (#10512)`), 15 de agosto de 2026
**Executor comparado com:** `release/v3.8.50` @ `976d670ff3a7712df0c695f13095c43eace5e29b` (fixado no relatório [`omniroute-chatgpt-web-reverse-engineering.md`](omniroute-chatgpt-web-reverse-engineering.md))
**Data da inspeção:** 15 de agosto de 2026
**Idioma:** português do Brasil
**Escopo:** leitura estática do código-fonte. Nenhuma execução autenticada, nenhum cookie, token ou conta real. Complementa o relatório do executor (07/ago) com a camada **acima** dele: seleção de credenciais, pool de contas, classificação de falhas, lockout/cooldown e a lane Codex OAuth que o OmniRoute já implementa.

---

## 1. Resumo executivo

O relatório anterior documentou o `ChatGptWebExecutor` (o *transporte*: cookie → JWT → Sentinel → PoW → `/backend-api/f/conversation` → SSE). Este relatório documenta o que acontece **antes e depois** dele no OmniRoute — a camada que decide *qual conta* atende o request e *o que fazer quando falha*:

1. **O executor está estável desde o relatório anterior.** O blob de `open-sse/executors/chatgpt-web.ts` tem o **mesmo hash** (`0168d07de2eb421e64db7ea69c9cb14f757c29e7`) em `release/v3.8.50` (07/ago) e no `main` de hoje (15/ago). Nenhuma mudança no caminho crítico do ChatGPT Web em 8 dias. O relatório anterior permanece válido integralmente.
2. **O pool é dirigido por classificação de falhas por sinais, não por status HTTP.** `open-sse/services/accountFallback.ts` (2.121 linhas) mantém listas de sinais de texto/erro para 6 categorias (conta desativada, créditos exaustos, token OAuth inválido, estouro de contexto, modelo sem acesso, rate limit) e classifica cada falha antes de decidir o remédio.
3. **Circuit breaker por modelo escopado a cota, não por conta.** A chave de lockout é `canonicalProvider:connectionId:lockModel` onde `lockModel` é:
   - o modelo exato para erros 404/`not_found` (modelo literal não existe);
   - o *scope de cota* do provedor: `family:gemini|claude|other` para Antigravity; `codex|spark` para Codex;
   - caso contrário, o modelo literal.
   `reason` e `status` influenciam a escolha do scope mas **não** são componentes persistidos da chave. Cooldown escalado com exponential backoff, honra de reset do upstream (`Retry-After`, `X-RateLimit-Reset`, "Resets in 160h"), `quota_exhausted` → trava até o reset conhecido (meia-noite para janelas diárias; para Codex Spark, janela semanal mapeada), decay da contagem de falhas, evicção de overflow (cap), e lock "exato" (`exactModelLock`) para tuplas literais que não devem afetar a família.
4. **O loop de execução está no `chatCore`**: `while (attempts < maxAttempts)` com **exclusão de connection IDs** entre tentativas (um request falho não re-tenta na mesma conta) e um failover dedicado para Codex (`codexFailover.ts` — marca o scope `codex|spark` como rate-limited até o reset informado).
5. **O OmniRoute já tem a lane Codex OAuth de primeira parte** (`src/lib/oauth/providers/codex.ts`, `codexDeviceFlow.ts`, `src/mitm/targets/codex.ts`): login via OAuth com `id_token` contendo `chatgpt_account_id`, `chatgpt_plan_type` e workspace; `codexResetCredits.ts`/`quotaShareConsumption.ts` rastreiam cotas flat-rate. O `codexDeviceFlow.ts` roda **ENTIRELY in the user's browser** (browser-bundled); o backend apenas recebe os tokens finais para persistência. **Não prova login puramente headless/server-side para #41** — nova autenticação via device flow exige o browser do usuário.
6. **Gap para o caso free do Runstead:** o pool do OmniRoute foi desenhado para provedores com rate limits por janela e créditos (429 → cooldown). A hipótese de semântica free do ChatGPT (teto **semanal**, modelo único GPT-5.6 Luna, "unlimited subject to abuse guardrails") **não é fato observado live** — a própria #58 exige evidência live antes de tratar a transição como observada. A documentação oficial atual da OpenAI informa GPT-5.5 Instant como padrão no ChatGPT, Free com limites dinâmicos em janela de 5h, e Luna fora das conversas padrão (fonte: *ChatGPT Free Tier FAQ*, OpenAI Help Center, inspecionado em 15/ago/2026) — portanto o relatório não transforma hipótese não observada em contrato de governor/reset semanal. O tratamento de `quota_exhausted` (trava até reset conhecido) é o análogo mais próximo, mas a contabilidade do teto (semanal, janela de 5h, ou outro) é responsabilidade do governor do Runstead (#58) e deve ser baseada em evidência live, não em hipótese.

---

## 2. Verificação de estabilidade do executor

| Campo | Valor |
|---|---|
| Hash `chatgpt-web.ts` em `release/v3.8.50` | `0168d07de2eb421e64db7ea69c9cb14f757c29e7` |
| Hash `chatgpt-web.ts` em `main` @ `aa5b77e` | `0168d07de2eb421e64db7ea69c9cb14f757c29e7` |
| Conclusão | **Idêntico** — o relatório de 07/ago continua sendo a fonte para o executor |

Consequência prática para o Runstead: a versão pinada do executor (doc anterior) e a versão atual são a mesma; o **drift a monitorar não é o código do OmniRoute** (que o Runstead não controla de qualquer forma), mas o **frontend do ChatGPT** (valores `OAI-Client-Version`/`OAI-Build-Number` capturados de abril/2026, `sentinel/sdk.js`, formato do PoW) — que é o objeto do drift probe proposto na issue nova.

---

## 3. Classificação de falhas por sinais (o cérebro do pool)

`accountFallback.ts` define **listas de sinais** e classificadores:

| Categoria | Constante | Sinais típicos | Remédio |
|---|---|---|---|
| Conta desativada/banida | `ACCOUNT_DEACTIVATED_SIGNALS` | `account_deactivated`, `account has been disabled`, `account has been suspended`, `verify your account to continue` (AG), `this service has been disabled … for violation` | Terminal: conta morta, eliminar do pool |
| Créditos exaustos | `CREDITS_EXHAUSTED_SIGNALS` | `insufficient_quota`, `billing_hard_limit_reached`, `exceeded your current quota`, `free tier of the model has been exhausted`, `tier has been exhausted` (âncora estreita para não capturar o "resource has been exhausted" transitório da Gemini) | Terminal até recarga |
| Token OAuth inválido | `OAUTH_INVALID_TOKEN_SIGNALS` | `invalid authentication credentials`, `login cookie`, `invalid credentials` | Re-auth (não ban) |
| Estouro de contexto | `CONTEXT_OVERFLOW_PATTERNS` | regex `input is too long`, `context.*(too long|exceeded|overflow)`, `token limit` | Fallback de modelo (janela maior) — **não** punir conta |
| Modelo sem acesso | `MODEL_ACCESS_DENIED_PATTERNS` + códigos estruturados | `model_not_found`, `deployment_not_found`, `not_found_error` (Anthropic), regex `model.*not.*(available|found|supported)` | Fallback de conta (PRO vs free pode mudar acesso) |
| Rate limit | `RATE_LIMIT_TEXT_PATTERNS` | 429 com texto | Cooldown curto, não terminal |

Observações de design que valem para o Runstead:

- **`isCreditsExhausted` é âncora em "tier"** por um motivo explícito no código (#8631): a frase genérica "has been exhausted" aparece em 429 *transitórios* da Gemini; classificar como terminal queimaria contas saudáveis. Lição: **sinais devem ser ancorados no mínimo necessário**, e uma categoria errada (terminal vs transitório) é mais cara que uma falha de request.
- **Classificação por texto + código estruturado juntos**: `MODEL_ACCESS_DENIED_CODES` (códigos de erro do schema, confiáveis) + `MODEL_ACCESS_DENIED_PATTERNS` (regex, para os que não têm código). Código estruturado primeiro, regex como fallback.
- **Categorias ambíguas são tratadas como tais**: `permission_error` da Anthropic só conta como modelo-sem-acesso se o texto confirmar; senão, é erro de auth/org/feature. Não existe "catch-all".
- Sinais customizáveis em runtime (`setCustomBannedSignals` — banidos carregados do DB) — o Runstead pode carregar sinais do ChatGPT Web por config/issue sem rebuild.

---

## 4. Model lockout: cooldown escalado, decay e evicção

O circuito por modelo (`modelLockouts` Map + `modelFailureState`) é o mecanismo mais maduro da camada:

- **Chave**: `canonicalProvider:connectionId:lockModel` (retornado por `getModelLockKey`), onde `lockModel` é:
  - o modelo exato para erros 404/`not_found` (modelo literal não existe);
  - o *scope de cota* do provedor: `family:gemini|claude|other` para Antigravity (`getQuotaScopedModelForProvider`); `codex|spark` para Codex (`getCodexModelScope`);
  - caso contrário, o modelo literal.
  `reason` e `status` **influenciam a escolha do scope** (ex.: `reason === "not_found" || status === 404` força modelo literal; `canonicalProvider === "codex"` usa scope Codex) mas **não são componentes persistidos da chave**. O lockout é por modelo-escopado-a-cota por conta, não por conta inteira.
- **Cooldown escalado** (`recordModelLockoutFailure`):
  - Exponential backoff por contagem de falhas (padrão);
  - **Honra de reset do upstream** (`selectLockoutCooldownMs` + `exactCooldownIsUpstreamReset`): se o erro trouxe `Retry-After`, `X-RateLimit-Reset` ou "Resets in 160h" parseado, o cooldown respeita **exatamente** esse valor — mesmo acima do teto (`maxCooldownMs`). Um "resets in 92h" não é achatado para minutos, senão o router martela 429 contra cota sabidamente ausente (#7940);
  - Estimativas **sintéticas** (backoff puro, quota-exhausted até reset conhecido) ficam sujeitas ao teto;
  - **`quota_exhausted` → trava até o reset conhecido** (`getMsUntilTomorrow` para janelas diárias; para Codex Spark, janela semanal mapeada via `toCodexScopedQuotaWindowName`).
- **Preserva o cooldown mais longo** entre lock concorrente (sem mutex: atômico no event loop single-threaded do Node; no Go do Runstead, exige mutex ou CAS).
- **Decay** (`decayModelFailureCount`): falhas antigas expiram pela janela (`getFailureWindowMs`, padrão 30 min), então a contagem não acumula para sempre.
- **Evicção de overflow** (`evictModelLockoutOverflow`, cap `MODEL_LOCKOUT_EVICTION_CAP`): o mapa não cresce sem limite.
- **Lock exato** (`exactModelLock.ts`): `lockExactModel` trava apenas o tuple exato `(provider, connectionId, model)` — não a "família de cota" — para casos onde o erro é escopado ao modelo literal (ex.: um alias específico).
- **Per-model quota** (`lockModelIfPerModelQuota`): quando o provedor tem cota por modelo, o lockout aplica antes de gastar o request.

---

## 5. Breaker por provedor e cooldown de conexão

- `getProviderBreaker` / `isProviderInCooldown` / `getProviderCooldownRemainingMs` / `getProviderBreakerState`: **circuit breaker por provedor** (todos os modelos/contas), configurável (`configureProviderBreaker`).
- `lockModel` + `provider` separados: um provedor inteiro pode entrar em cooldown (ex.: outage upstream) sem tocar nos lockouts por modelo.
- `cooldownCap.ts` (26 linhas): teto do cooldown aplicado.
- `lockoutEviction.ts` (53 linhas): política de evicção do lockout overflow.
- `nonRetryableUpstream.ts` (100 linhas): lista de erros upstream que **não devem ser retentados** (ex.: CF 1010 do #8775 — "The owner of this website has banned you").
- `getRuntimeProviderProfile` / `ProviderProfile`: perfis por provedor com janela de falha e cooldown base configuráveis por provider.

---

## 6. Loop de execução e fallback de contas (`chatCore`)

`open-sse/handlers/chatCore.ts` (rota de execução única para os 3 formatos públicos):

- **Loop de tentativas** (~linha 2770): `while (attempts < maxAttempts)` com **exclusão de connection IDs** — um request que falha em uma conta não re-tenta na mesma (a conta sai do conjunto candidato para aquela tentativa).
- **`codexFailover.ts`**: failover dedicado da lane Codex (tenta outra conexão Codex em caso de falha da primeira), com rastreamento explícito dos IDs excluídos.
- `executionCredentials.ts` / `requestSetup.ts`: montagem das credenciais efetivas por request; `connectionId` é injetado nas credenciais para o executor poder rotacionar.
- `idempotency.ts`: o OmniRoute tem módulo de idempotência — relevante para o contrato de attempt receipts do Runstead (#29).
- **O executor chatgpt-web não participa do loop de retry do `BaseExecutor`** (documentado no relatório anterior) — a serialização/fallback acontece neste loop do `chatCore`, acima do executor. O `ChatGptWebExecutor` é stateless entre requests (exceto caches bounded).

---

## 7. Lane Codex OAuth (o "caminho oficial" já existe no OmniRoute)

- `src/lib/oauth/providers/codex.ts`: fluxo OAuth do Codex com `id_token` JWT contendo claims custom em `https://api.openai.com/auth` (`chatgpt_account_id`, `chatgpt_plan_type`, `chatgpt_user_id`, `organizations[].{id, is_default, role, title}`) — o workspace selecionado na tela de autorização chega embutido no token.
- `src/lib/oauth/codexDeviceFlow.ts`: **device code flow executado inteiramente no navegador do usuário** — o comentário no código é explícito: o fluxo roda **ENTIRELY in the user's browser** (o módulo é browser-bundled, livre de imports server-only). O código do browser faz: (1) `POST /api/accounts/deviceauth/usercode` → `{ deviceAuthId, userCode, interval }`; (2) o usuário abre `https://auth.openai.com/codex/device` e digita o `userCode`; (3) polling `POST /api/accounts/deviceauth/token` até obter `{ authorization_code, code_verifier }`; (4) `POST /oauth/token` (form) com `authorization_code` + `code_verifier` + `redirect_uri` → `{ access_token, refresh_token, id_token, expires_in }`. **Só após a conclusão no browser** os tokens finais são entregues ao backend OmniRoute para persistência. **Não prova login puramente headless/server-side para #41** — o step "browser do usuário" é obrigatório para nova autenticação via device flow.
- `src/lib/oauth/utils/codexAuthFile.ts`, `codexSessionImport.ts`, `codexAuthImport.ts`: importa o `auth.json` do Codex CLI (`~/.codex/auth.json`) como fonte de credencial — `codexAuthImport.ts` aceita tokens já existentes e cria/atualiza uma conexão OmniRoute **sem executar o device flow**. Logo, a importação de sessão já autenticada **não exige** o browser-driven flow; apenas a **nova autenticação** via device flow exige.
- `src/mitm/targets/codex.ts` + `src/mitm/handlers/codex.ts`: o OmniRoute também serve Codex via MITM de proxy.
- `src/lib/usage/codexResetCredits.ts`, `flatRateProviders.ts`, `quotaShareConsumption.ts`: o OmniRoute **já rastreia créditos/reset do Codex** (cota flat-rate compartilhada, scopes `codex` e `spark` com janelas `session`/`weekly` mapeadas) — pré-requisito de contabilidade que o governor do Runstead (#58) pode espelhar.
- `markCodexScopeRateLimited` (`codexFailover.ts`): failover dedicado da lane Codex — em 429, marca o scope (`codex|spark`) como rate-limited até o `rateLimitedUntil` informado pelo upstream; na próxima tentativa, exclui conexões com scope bloqueado.

Implicação para o bake-off (#18/M7): a existência da lane Codex OAuth dentro do OmniRoute é um **achado relevante** (alternativa futura, roteamento interno), mas **não redefine o gate do bake-off**. O README, `docs/roadmap.md` e a própria #18 definem o bake-off entre `provider/omniroute` (reverse-engineered ChatGPT Web) e o first-party `provider/chatgptweb` (ainda não implementado). A lane Codex pode ser registrada como alternativa futura; não muda o gate sem decisão arquitetural explícita e evidência live. Também não há base nesta inspeção estática para chamar essa lane de "estável" em contraste com o first-party path ainda não implementado.

---

## 8. O que copiar vs rejeitar no Runstead

### Copiar (conceitos, reimplementar em Go)

| Conceito | Fonte | Para |
|---|---|---|
| Classificação de falhas por sinais (terminal vs transitório vs conta-específico) | `accountFallback.ts` §3 | Governor #58 + telemetria #39 |
| Sinais ancorados no mínimo (lição `tier has been exhausted`) | `CREDITS_EXHAUSTED_SIGNALS` | Classificador do Runstead |
| Lockout por `(conta, lockModel)` com cooldown escalado + backoff | `recordModelLockoutFailure` | Governor #58 |
| Honra de reset do upstream (exato, acima do teto) vs estimativa (teto) | `exactCooldownIsUpstreamReset` | Governor #58 |
| `quota_exhausted` → trava até reset conhecido (meia-noite diária, semanal para Codex Spark) | `getMsUntilTomorrow`, `toCodexScopedQuotaWindowName` | Governor #58 |
| Decay de contagem de falhas + evicção (cap) | `decayModelFailureCount`, `evictModelLockoutOverflow` | Governor #58 |
| Exclusão de connection IDs entre tentativas do loop | `chatCore` while-loop | Provider adapter (omniroute lane) |
| Import de `~/.codex/auth.json` (reuso de sessão já autenticada) | `codexAuthFile.ts` | Admissão de sessões (issue #41) |
| Contabilidade de reset de cota flat-rate (scopes `codex`/`spark`, janelas `session`/`weekly`) | `codexResetCredits.ts`, `quotaShareConsumption.ts` | Governor #58 |
| Failover por scope Codex (`markCodexScopeRateLimited`) | `codexFailover.ts` | Governor #58 |

### Rejeitar (não adotar no Runstead)

| Prática | Motivo |
|---|---|
| Heurística de texto como fonte primária (sem estrutura) | Runstead deve preferir campos estruturados do erro (status, `error.code`) e usar regex só como fallback |
| Lockout sem mutex apoiado em event loop single-threaded | Em Go, concorrência real exige mutex/CAS; o OmniRoute "escapa" por ser Node |
| Categorias catch-all / classificação ambígua como erro de conta | Custa contas saudáveis; o Runstead deve tratar ambíguo como *unverifiable* e não punir a conta |
| Escopo do monólito (dashboard + gateway + MCP + memória) | já rejeitado no relatório anterior |
| Dependência de sinais de texto específicos de outros providers (Gemini/Anthropic) | não se aplicam ao ChatGPT Web; manter lista mínima própria |

---

## 9. Gap free-tier: o que o OmniRoute não resolve para o Runstead

O pool do OmniRoute foi desenhado para **provedores com créditos/rate limits por janela**. O caso free do ChatGPT (decisão do projeto: web lane como provedor principal, contas free) tem semântica que **não é fato observado live** — a própria #58 exige evidência live antes de tratar a transição como observada. A documentação oficial atual da OpenAI informa GPT-5.5 Instant como padrão no ChatGPT, Free com limites dinâmicos em janela de 5h, e Luna fora das conversas padrão. O que se sabe da implementação do OmniRoute:

1. **Reset de cota conhecido**: o `quota_exhausted` do OmniRoute trava até o reset conhecido (meia-noite para janelas diárias; janela semanal mapeada para Codex Spark). É o análogo mais próximo, mas a contabilidade do teto (semanal, janela de 5h, ou outro) é responsabilidade do governor do Runstead (#58) e deve ser baseada em evidência live, não em hipótese.
2. **Modelo único (se confirmado)**: se a conta free realmente tiver um único modelo, o fallback por modelo do OmniRoute (lockout por `lockModel`) não se aplica — o fallback seria entre contas, e o lockout por modelo vira lockout por conta/sessão.
3. **"Unlimited subject to abuse guardrails"**: se o limitante real for o Sentinel/anti-bot (não o rate limit), o lockout por sinais do OmniRoute não detecta "sentinel começou a exigir Turnstile" — isso é o drift probe + classificação `SENTINEL_BLOCKED` (já mapeada no relatório do executor).
4. **Custo de admissão alto**: cada conta free nova custa login + verificação + Turnstile. O pool do OmniRoute assume conexões baratas de criar; o Runstead precisa tratar sessão como ativo caro (warming, custódia, reuso — issue #41).

---

## 10. Implicações para as issues do Runstead

| Issue | Impacto dos achados |
|---|---|
| **#58** (governor Luna) | O `accountFallback` é o modelo de referência completo: classificação por sinais, lockout por `lockModel` (scope de cota) com cooldown escalado, honra de reset upstream, decay, evicção, failover por scope Codex. Adaptações: chave baseada no `lockModel` do provedor; reset conhecido (não hipótese semanal); falha `SENTINEL_BLOCKED` como categoria própria; contabilidade baseada em evidência live. |
| **#39** (telemetria mínima) | O motivo da falha deve ser a **categoria classificada** (conta_banida / creditos_exaustos / sentinel / token_invalido / drift / timeout), não só o status HTTP — é o que permite calibrar pool e detectar guardrail antes do usuário. Lição: sinais ancorados no mínimo (ex.: `tier has been exhausted`). |
| **#18** (bake-off/M7) | OmniRoute tem a lane Codex OAuth (browser-driven flow, import de auth.json, contabilidade de reset/scopes). É achado relevante (alternativa futura, roteamento interno), mas **não redefine o gate** entre `provider/omniroute` e first-party `provider/chatgptweb`. |
| **#16/#17** (first-party adapter) | O loop com exclusão de connection IDs e o tratamento de falha por categoria são o contrato esperado do adapter. |
| **Drift probe (nova #74)** | O executor está estável no OmniRoute (hash idêntico), mas os valores de frontend do ChatGPT (OAI-Client-Version/Build de abril/2026) continuam codificados sem detector — formalizar a issue de probe com base no relatório anterior. |

---

## 11. Componentes e SHAs de referência

| Arquivo (main @ `aa5b77e`) | Linhas | Papel |
|---|---|---|
| `open-sse/executors/chatgpt-web.ts` | 3.240 | Executor (idêntico a v3.8.50; já documentado) |
| `open-sse/services/accountFallback.ts` | 2.121 | Classificação de falhas + lockout por modelo + breaker |
| `open-sse/services/accountFallback/{cooldownCap,exactModelLock,lockoutEviction,nonRetryableUpstream}.ts` | 337 | Módulos do pool |
| `open-sse/handlers/chatCore.ts` (+ `chatCore/`) | — | Rota única de execução; loop com exclusão de connection IDs |
| `open-sse/handlers/chatCore/{executionCredentials,idempotency,codexFailover,quotaShareConsumption}.ts` | — | Credenciais, idempotência, failover Codex |
| `src/lib/oauth/providers/codex.ts` | — | OAuth Codex (id_token com workspace) |
| `src/lib/oauth/codexDeviceFlow.ts` | — | Device code flow browser-driven (requer navegador do usuário) |
| `src/lib/oauth/utils/codexAuthFile.ts` | — | Import de `~/.codex/auth.json` |
| `src/lib/usage/codexResetCredits.ts` | — | Contabilidade de reset da cota Codex |
| `tests/unit/account-fallback-*.test.ts` | — | Suite de testes do pool (referência de casos) |
