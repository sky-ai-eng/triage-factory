# Changelog

## [1.12.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.12.0...v1.12.1) (2026-06-18)


### Bug Fixes

* **headless:** env-driven local-mode bootstrap (TFAC-411) ([#428](https://github.com/sky-ai-eng/triage-factory/issues/428)) ([43007af](https://github.com/sky-ai-eng/triage-factory/commit/43007af44271efdbcd9cf4e0cbd4026fd9637457))

## [1.12.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.11.0...v1.12.0) (2026-06-18)


### Features

* **board:** surface permission prompts inline (TFAC-391) ([#407](https://github.com/sky-ai-eng/triage-factory/issues/407)) ([7cf7829](https://github.com/sky-ai-eng/triage-factory/commit/7cf78291de853bb20d9d5f6ac89beebc0fe34f03))
* **exec:** persist pr diff to a file + manifest (TFAC-390) ([#406](https://github.com/sky-ai-eng/triage-factory/issues/406)) ([fbba7f1](https://github.com/sky-ai-eng/triage-factory/commit/fbba7f1fc2cbdffe14bf7d04c268969b7431fbfa))
* **exec:** slim pr files to a summary envelope (TFAC-393) ([#408](https://github.com/sky-ai-eng/triage-factory/issues/408)) ([f3782c8](https://github.com/sky-ai-eng/triage-factory/commit/f3782c88b8a6658b63677db59bbb1b0ce00f991a))
* **jira:** Atlassian OAuth app config (TFAC-337) ([#399](https://github.com/sky-ai-eng/triage-factory/issues/399)) ([0c19768](https://github.com/sky-ai-eng/triage-factory/commit/0c19768c460b1cc8ad910a0dda35d32fb8f3f460))
* **jira:** cloud org service onboarding (TFAC-336 & TFAC-340) ([#393](https://github.com/sky-ai-eng/triage-factory/issues/393)) ([539ea57](https://github.com/sky-ai-eng/triage-factory/commit/539ea57368f940401182e5531696a3c2c4eccefc))
* **jira:** Cloud per-user API-token bind (TFAC-338) ([#400](https://github.com/sky-ai-eng/triage-factory/issues/400)) ([fc8ef8c](https://github.com/sky-ai-eng/triage-factory/commit/fc8ef8c3257496c4a84ee77d4bd7bcb9bda4a5fc))
* **jira:** Cloud per-user OAuth 3LO "Connect" (TFAC-339) ([#401](https://github.com/sky-ai-eng/triage-factory/issues/401)) ([3e372f8](https://github.com/sky-ai-eng/triage-factory/commit/3e372f80fdcdde5d7bae997cd01bb276784f25ce))
* **jira:** multi-mode background polling (TFAC-387) ([#403](https://github.com/sky-ai-eng/triage-factory/issues/403)) ([6163a19](https://github.com/sky-ai-eng/triage-factory/commit/6163a1953b80d770920f6f8a557b5fb9da7bef13))
* **secrets:** app-layer AEAD secret store, replacing Supabase Vault (TFAC-402) ([#415](https://github.com/sky-ai-eng/triage-factory/issues/415)) ([f1c8db1](https://github.com/sky-ai-eng/triage-factory/commit/f1c8db1c3fa3cb6f8959252ef232c262385d4755))
* **secrets:** bind org_secrets ciphertext to its row identity (TFAC-403) ([#421](https://github.com/sky-ai-eng/triage-factory/issues/421)) ([c2aa29c](https://github.com/sky-ai-eng/triage-factory/commit/c2aa29cd8600fa69f69e46111d52b7eb24080167))
* **secrets:** local headless encrypted file secret backend (TFAC-404) ([#417](https://github.com/sky-ai-eng/triage-factory/issues/417)) ([9f124d6](https://github.com/sky-ai-eng/triage-factory/commit/9f124d608bdcf26ff6dde88ec30bf8525f50b910))
* **settings:** org Claude credentials (TFAC-386) ([#402](https://github.com/sky-ai-eng/triage-factory/issues/402)) ([a4d0dba](https://github.com/sky-ai-eng/triage-factory/commit/a4d0dbac825422244581ab76f01d54fd8b214afa))


### Bug Fixes

* **agentmeta:** sum blueprint cost across all steps, drop model name (TFAC-388) ([#404](https://github.com/sky-ai-eng/triage-factory/issues/404)) ([9691333](https://github.com/sky-ai-eng/triage-factory/commit/9691333edd80959e617ad23f7f7bb266f10bb16c))
* **agentproc:** pin libc-matched native binary in SDK wrapper ([#418](https://github.com/sky-ai-eng/triage-factory/issues/418)) ([5cc65b1](https://github.com/sky-ai-eng/triage-factory/commit/5cc65b1f67c9843d9cdc29c73e2cc42af522064d))
* **dashboard:** show PRs/stats for App-mode orgs; seed history (TFAC-396) ([#413](https://github.com/sky-ai-eng/triage-factory/issues/413)) ([d20b87c](https://github.com/sky-ai-eng/triage-factory/commit/d20b87cc26e8069de5e8b8f9f1011a787ea570f4))
* **db:** dedupe goose migration version collision on main (202606170001) ([77fcfbd](https://github.com/sky-ai-eng/triage-factory/commit/77fcfbde16c0e07301a562f0596dff0bf7d76630))
* **httpx:** treat client-canceled requests as 499, not 500 (TFAC-398) ([#426](https://github.com/sky-ai-eng/triage-factory/issues/426)) ([a68b8c9](https://github.com/sky-ai-eng/triage-factory/commit/a68b8c97ac5d0aa29e28744479dc08e1d00878a8))
* **paths:** decouple image-baked toolchain from the data state root (TFAC-394) ([#409](https://github.com/sky-ai-eng/triage-factory/issues/409)) ([6edd1f8](https://github.com/sky-ai-eng/triage-factory/commit/6edd1f80f694265e9b466ef658b7a474fa331ce8))
* **prompts:** theme-aware trigger-label pill on the binding canvas (TFAC-400) ([#410](https://github.com/sky-ai-eng/triage-factory/issues/410)) ([8677801](https://github.com/sky-ai-eng/triage-factory/commit/867780181eced00484aa135ea616ff84d53266ce))
* **review:** mechanize --severity so badges render (TFAC-389) ([#405](https://github.com/sky-ai-eng/triage-factory/issues/405)) ([e3d0003](https://github.com/sky-ai-eng/triage-factory/commit/e3d0003d147016645a6ebfcc4124fd68bd388291))
* **sandbox:** launch runsc with --host-uds=open (TFAC-407) ([#424](https://github.com/sky-ai-eng/triage-factory/issues/424)) ([3a37bd7](https://github.com/sky-ai-eng/triage-factory/commit/3a37bd7a3f1e4df254d8d2587d67139b98070ffc))
* **ui:** theme-aware primary button fill for dark mode (TFAC-395) ([#411](https://github.com/sky-ai-eng/triage-factory/issues/411)) ([34b0dba](https://github.com/sky-ai-eng/triage-factory/commit/34b0dbab71334539e82fa502d7f1d4fea9d2ccad))
* **uninstall:** sweep anthropic + jira-oauth + GitHub App keychain keys (TFAC-405) ([#422](https://github.com/sky-ai-eng/triage-factory/issues/422)) ([d253f1e](https://github.com/sky-ai-eng/triage-factory/commit/d253f1efd93144856073dec4d76afe20e19ea2b6))
* **worktree:** authenticate host-side blobless checkout's promisor fetch (TFAC-401) ([#412](https://github.com/sky-ai-eng/triage-factory/issues/412)) ([41886cb](https://github.com/sky-ai-eng/triage-factory/commit/41886cb9e5049788cb564a5bb92c8a62613776da))
* **worktree:** stop logging an absent cache tier as a scan walk error (TFAC-397) ([#420](https://github.com/sky-ai-eng/triage-factory/issues/420)) ([fd8e925](https://github.com/sky-ai-eng/triage-factory/commit/fd8e925344626642edd6a9355f33c870cf604c81))

## [1.11.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.10.1...v1.11.0) (2026-06-15)


### Features

* **agentproc:** browser steering for sandbox runs (TFAC-323) ([#377](https://github.com/sky-ai-eng/triage-factory/issues/377)) ([50360ea](https://github.com/sky-ai-eng/triage-factory/commit/50360ea0ba01c6c93eb8f14009e0464b7332e10b))
* **agentproc:** interactive streaming-input bridge (TFAC-304) ([#346](https://github.com/sky-ai-eng/triage-factory/issues/346)) ([158bb20](https://github.com/sky-ai-eng/triage-factory/commit/158bb208f72bc4ce223d9d8e89dc8caa5960c88c))
* **agentproc:** per-org credential resolver (SKY-322) ([#213](https://github.com/sky-ai-eng/triage-factory/issues/213)) ([1d8d869](https://github.com/sky-ai-eng/triage-factory/commit/1d8d869595ec2d827250786640366f665011375c))
* **agentproc:** thread orgID through Classify (SKY-323) ([#215](https://github.com/sky-ai-eng/triage-factory/issues/215)) ([38dcb1c](https://github.com/sky-ai-eng/triage-factory/commit/38dcb1cfffa0ec3cfc2d33b1783f7323113faf5e))
* **agentproc:** wire LLM proxy into sandbox path (SKY-335) ([#220](https://github.com/sky-ai-eng/triage-factory/issues/220)) ([3e6cd6b](https://github.com/sky-ai-eng/triage-factory/commit/3e6cd6b9ecd9b444c5a9b54b4486dc5c96f6e03d))
* **agent:** switch runtime from claude CLI to Agent SDK ([#135](https://github.com/sky-ai-eng/triage-factory/issues/135)) ([7c2bdd4](https://github.com/sky-ai-eng/triage-factory/commit/7c2bdd4f93a25a4e654e9d747aa78ae942aa789e))
* **ai:** per-org scorer manager (SKY-324) ([#219](https://github.com/sky-ai-eng/triage-factory/issues/219)) ([eb782ee](https://github.com/sky-ai-eng/triage-factory/commit/eb782ee107aafd18afa91c5781435ff811c56cef))
* **auth:** active-org session persistence + switching (SKY-313) ([#199](https://github.com/sky-ai-eng/triage-factory/issues/199)) ([6a9c0b8](https://github.com/sky-ai-eng/triage-factory/commit/6a9c0b8a09d373e685d7407b3e8caefb7e231065))
* **auth:** add per-(user, org) WebSocket scoping (SKY-311) ([#206](https://github.com/sky-ai-eng/triage-factory/issues/206)) ([dabb41e](https://github.com/sky-ai-eng/triage-factory/commit/dabb41e3aa8c7eea0a99f0476e13180980045ce3))
* **auth:** orgID -&gt; router for multi-tenant events (SKY-320) ([#205](https://github.com/sky-ai-eng/triage-factory/issues/205)) ([66cf40e](https://github.com/sky-ai-eng/triage-factory/commit/66cf40e56c76de3c10de65d1462066cffaaed8ed))
* **auth:** personal org auto-provision on signup (SKY-345) ([#234](https://github.com/sky-ai-eng/triage-factory/issues/234)) ([b4194d8](https://github.com/sky-ai-eng/triage-factory/commit/b4194d83d7c1d959b16130a5e7a591a8589612c6))
* **auth:** refactor authenticated route registration (SKY-314) ([#202](https://github.com/sky-ai-eng/triage-factory/issues/202)) ([6e57a6a](https://github.com/sky-ai-eng/triage-factory/commit/6e57a6ac969b7033a1c7b9c1680faa7ab8fa83ee))
* **auth:** replace runmode defaults with org/user IDs (SKY-316) ([#203](https://github.com/sky-ai-eng/triage-factory/issues/203)) ([b06f522](https://github.com/sky-ai-eng/triage-factory/commit/b06f5221c79e48e1b9efaa1927e133dcd9bc5811))
* **auth:** SKY-250 – GoTrue substrate + JWKS verify (RS256) ([#171](https://github.com/sky-ai-eng/triage-factory/issues/171)) ([a295f0e](https://github.com/sky-ai-eng/triage-factory/commit/a295f0eef084c7d99c152c97c783e4864d042c4e))
* **auth:** SKY-251 – auth middleware + sessions ([#172](https://github.com/sky-ai-eng/triage-factory/issues/172)) ([4be3c6c](https://github.com/sky-ai-eng/triage-factory/commit/4be3c6c08ea182469f439683937d8e97c6b825e0))
* **auth:** synthesize response in local mode /api/me (SKY-315) ([#201](https://github.com/sky-ai-eng/triage-factory/issues/201)) ([0a7d7af](https://github.com/sky-ai-eng/triage-factory/commit/0a7d7afd2d5894f3816cb5fbac5afa99d8c87c3f))
* **auth:** thread orgID through delegation spawner (SKY-319) ([#204](https://github.com/sky-ai-eng/triage-factory/issues/204)) ([1231cf7](https://github.com/sky-ai-eng/triage-factory/commit/1231cf79f954fa6ed6be356172f8400706080e43))
* **auth:** wrap HTTP handlers in transaction context (SKY-325) ([#207](https://github.com/sky-ai-eng/triage-factory/issues/207)) ([7672f80](https://github.com/sky-ai-eng/triage-factory/commit/7672f800d7924ac083eb39895fb04db5fbe1cadc))
* **blueprint:** 1:1 blueprint:prompt, auto-wrap, soft-delete (SKY-430) ([#300](https://github.com/sky-ai-eng/triage-factory/issues/300)) ([e1b3f8e](https://github.com/sky-ai-eng/triage-factory/commit/e1b3f8e820bea47d41fa432db88d1513b644203f))
* **blueprint:** add blueprint deep-copy endpoint (SKY-432) ([#302](https://github.com/sky-ai-eng/triage-factory/issues/302)) ([00d467b](https://github.com/sky-ai-eng/triage-factory/commit/00d467bd3020cef7313246752c8d1bbc7bdd1bd8))
* **blueprint:** atomic merge/split endpoints + constraint (SKY-431) ([#301](https://github.com/sky-ai-eng/triage-factory/issues/301)) ([8a29729](https://github.com/sky-ai-eng/triage-factory/commit/8a29729b48e0aaa4958da7d303a1b6f48a7c4501))
* **blueprint:** blob storage for agent workspaces (SKY-421) ([#292](https://github.com/sky-ai-eng/triage-factory/issues/292)) ([3af5c1d](https://github.com/sky-ai-eng/triage-factory/commit/3af5c1d6e18a6f5e13c1200b80423b301b14c396))
* **blueprint:** blueprint merge/split and box UI (SKY-429) ([#303](https://github.com/sky-ai-eng/triage-factory/issues/303)) ([4306b1c](https://github.com/sky-ai-eng/triage-factory/commit/4306b1cea848260cb72542bc5f67430062cab716))
* **blueprint:** canvas select + duplicate prompts/blueprints (SKY-433) ([#306](https://github.com/sky-ai-eng/triage-factory/issues/306)) ([c36e4c2](https://github.com/sky-ai-eng/triage-factory/commit/c36e4c2519a7ace5fe7620791d48c02523a4a480))
* **blueprint:** canvas selection & delete-key interactions (SKY-453) ([#330](https://github.com/sky-ai-eng/triage-factory/issues/330)) ([4fa9e94](https://github.com/sky-ai-eng/triage-factory/commit/4fa9e94646364a0bca6a2e8c67bc802eef1ac772))
* **blueprint:** DB run queue + state-machine reactor (SKY-426) ([#317](https://github.com/sky-ai-eng/triage-factory/issues/317)) ([c120575](https://github.com/sky-ai-eng/triage-factory/commit/c1205752909922b451b7af5ec04e8d3d623993e1))
* **blueprint:** delete a whole blueprint (SKY-452) ([#326](https://github.com/sky-ai-eng/triage-factory/issues/326)) ([e575848](https://github.com/sky-ai-eng/triage-factory/commit/e575848f41d61f15ab9605306a911c53856615af))
* **blueprint:** delete prompts from multi-step blueprints (SKY-451) ([#324](https://github.com/sky-ai-eng/triage-factory/issues/324)) ([fa7084b](https://github.com/sky-ai-eng/triage-factory/commit/fa7084bcc2732b2d61973cd94d4bfcfafe568fc3))
* **blueprint:** master/detail picker(TFAC-342) ([#397](https://github.com/sky-ai-eng/triage-factory/issues/397)) ([f23c77b](https://github.com/sky-ai-eng/triage-factory/commit/f23c77be65a307d690b6ec1224772fc7a5a443f5))
* **blueprint:** MinIO object store in self-host compose (SKY-422) ([#293](https://github.com/sky-ai-eng/triage-factory/issues/293)) ([9b4876c](https://github.com/sky-ai-eng/triage-factory/commit/9b4876ca1cc00356996cddab46c42702d35ea3b5))
* **blueprint:** multi-step org blueprint templates (SKY-418) ([#289](https://github.com/sky-ai-eng/triage-factory/issues/289)) ([8d24bef](https://github.com/sky-ai-eng/triage-factory/commit/8d24befa792bd33218fb3420f767308ceb4e20b3))
* **blueprint:** namespace run memory by blueprint_run + unify completion gate (SKY-420) ([#291](https://github.com/sky-ai-eng/triage-factory/issues/291)) ([472d1f5](https://github.com/sky-ai-eng/triage-factory/commit/472d1f5a035028e50eaea6bae4b4f35326277a3b))
* **blueprint:** position-gated outcome contract + parser + runs.outcome (SKY-417) ([#287](https://github.com/sky-ai-eng/triage-factory/issues/287)) ([0164e30](https://github.com/sky-ai-eng/triage-factory/commit/0164e307b6a96d2acf725ea3bf1179790c7921a7))
* **blueprint:** universal blueprint_run + dispatch (SKY-425) ([#315](https://github.com/sky-ai-eng/triage-factory/issues/315)) ([4ca5dbf](https://github.com/sky-ai-eng/triage-factory/commit/4ca5dbfe64094e79ce28f2b88081af9f455bbc48))
* **board:** five-column board redesign (SKY-330) ([#212](https://github.com/sky-ai-eng/triage-factory/issues/212)) ([0e2ce29](https://github.com/sky-ai-eng/triage-factory/commit/0e2ce294312764f81d508eb74dd3db66c59fff4b))
* **board:** full-screen agent session view ([#155](https://github.com/sky-ai-eng/triage-factory/issues/155)) ([87cfe44](https://github.com/sky-ai-eng/triage-factory/commit/87cfe44ec1cff38e2fbe759c60ffa0a68be7c0ed))
* **board:** liquid-glass + HUD redesign (TFAC-308) ([#350](https://github.com/sky-ai-eng/triage-factory/issues/350)) ([57fbaf7](https://github.com/sky-ai-eng/triage-factory/commit/57fbaf7bfbf8e58f041af759112d25822d52c3e3))
* **chain:** prompt chaining — linear multi-step delegation ([#164](https://github.com/sky-ai-eng/triage-factory/issues/164)) ([ee5fc5e](https://github.com/sky-ai-eng/triage-factory/commit/ee5fc5e37f741b35e4b36135d68678b6fa90e363))
* **cloud:** add git and openssh-client to runtime (SKY-390) ([#271](https://github.com/sky-ai-eng/triage-factory/issues/271)) ([623052c](https://github.com/sky-ai-eng/triage-factory/commit/623052c65220829ad678c6120150aefff5f09014))
* **cloud:** add middleware shim for local mode (SKY-253) ([#198](https://github.com/sky-ai-eng/triage-factory/issues/198)) ([98ffdcd](https://github.com/sky-ai-eng/triage-factory/commit/98ffdcdb8ac04bc19e4593125bc434bcf84546ae))
* **cloud:** add org-scoped event bus routing (SKY-310) ([#197](https://github.com/sky-ai-eng/triage-factory/issues/197)) ([bb9c734](https://github.com/sky-ai-eng/triage-factory/commit/bb9c734ffe72e38846768bb3b4e27fad6213b7c8))
* **cloud:** docker, Fly.io, and multi-arch support (SKY-256) ([#224](https://github.com/sky-ai-eng/triage-factory/issues/224)) ([5f0e648](https://github.com/sky-ai-eng/triage-factory/commit/5f0e648a4035f6f1ba086f55eb4dccebc868c090))
* **cloud:** Postgres pools, auth, sessions, RLS (SKY-339) ([#226](https://github.com/sky-ai-eng/triage-factory/issues/226)) ([b2f1ad2](https://github.com/sky-ai-eng/triage-factory/commit/b2f1ad2565b931c998b2b3aa0d6718ffe93ff2e7))
* **cloud:** thread org/team through cmd/exec (SKY-340) ([#229](https://github.com/sky-ai-eng/triage-factory/issues/229)) ([e0f261e](https://github.com/sky-ai-eng/triage-factory/commit/e0f261e2323c1dba8f212016a1d9acefd25ebe01))
* **cmd/exec:** SKY-302 run identity + synthetic-claims routing ([#195](https://github.com/sky-ai-eng/triage-factory/issues/195)) ([aaa71bf](https://github.com/sky-ai-eng/triage-factory/commit/aaa71bfa788c00e57a98e6874d7270afc53412fd))
* **creds:** per-org run-credential resolution seam (SKY-389) ([077d353](https://github.com/sky-ai-eng/triage-factory/commit/077d3538b65a486b941aa6544d953050b24e4ceb))
* **creds:** require captured model on resume, no live-resolve fallback (SKY-389) ([f15f893](https://github.com/sky-ai-eng/triage-factory/commit/f15f8934f0ee1705077be1ce08ddee9ec99e0069))
* **creds:** resolve run model per-(org, team), not org default (SKY-389) ([c5688d3](https://github.com/sky-ai-eng/triage-factory/commit/c5688d3424a89daf373f88b9696663e84ed19fcb))
* **curator:** in-sandbox exec daemon + shared read-only pinned-repo mounts (TFAC-61) ([#386](https://github.com/sky-ai-eng/triage-factory/issues/386)) ([3999959](https://github.com/sky-ai-eng/triage-factory/commit/39999592839a038b874c4d3301537599381050fd))
* **curator:** route on-disk state through org-scoped internal/paths (SKY-402) ([#284](https://github.com/sky-ai-eng/triage-factory/issues/284)) ([6ed384a](https://github.com/sky-ai-eng/triage-factory/commit/6ed384a3b75f8af780f904fb14e9510b6e3da39f))
* **curator:** SKY-298 — per-turn synthetic-claims for goroutine writes ([#194](https://github.com/sky-ai-eng/triage-factory/issues/194)) ([ba46b0c](https://github.com/sky-ai-eng/triage-factory/commit/ba46b0c0991fc148c5b554d31655cd5868cd66b5))
* **db:** add admin-pool routes for cross-org defense ([#189](https://github.com/sky-ai-eng/triage-factory/issues/189)) ([fcd6eac](https://github.com/sky-ai-eng/triage-factory/commit/fcd6eac4a55f147bca029e5d8a029e6ecb13415d))
* **db:** add admin-pool variants for background goroutines ([#185](https://github.com/sky-ai-eng/triage-factory/issues/185)) ([75b86b1](https://github.com/sky-ai-eng/triage-factory/commit/75b86b1735c74bc941729921d00158b4a5f77908))
* **db:** add claims-free identity -&gt; routing lookup (SKY-405) ([#276](https://github.com/sky-ai-eng/triage-factory/issues/276)) ([8a005da](https://github.com/sky-ai-eng/triage-factory/commit/8a005da1c623c26e9afd968690cc2668fa575155))
* **db:** add GetSystem path for system secret reads (SKY-364) ([#250](https://github.com/sky-ai-eng/triage-factory/issues/250)) ([0e040e4](https://github.com/sky-ai-eng/triage-factory/commit/0e040e4f7dc62081fbd6691bbb8d9aeac62ab3a9))
* **db:** add GitHub team -&gt; TF team mapping (SKY-369) ([#258](https://github.com/sky-ai-eng/triage-factory/issues/258)) ([122f0a4](https://github.com/sky-ai-eng/triage-factory/commit/122f0a4640b6419ee73720e7971064eee76eeb88))
* **db:** add team-membership filtering to factory (SKY-366) ([#254](https://github.com/sky-ai-eng/triage-factory/issues/254)) ([d2935f8](https://github.com/sky-ai-eng/triage-factory/commit/d2935f8293e7d4d5667049f29a78e2f8d213cb54))
* **db:** admin-pool routing and redundant tests ([#187](https://github.com/sky-ai-eng/triage-factory/issues/187)) ([072cb57](https://github.com/sky-ai-eng/triage-factory/commit/072cb5781f9cf09bbb52eb9a02e1dbe455341458))
* **db:** continue migration from raw SQL -&gt; stores ([#190](https://github.com/sky-ai-eng/triage-factory/issues/190)) ([533c6ee](https://github.com/sky-ai-eng/triage-factory/commit/533c6eeb7a48307fea9cb6f035a568bbcbb7b0e2))
* **db:** delete config package, migrate to stores (SKY-355) ([#240](https://github.com/sky-ai-eng/triage-factory/issues/240)) ([75b9050](https://github.com/sky-ai-eng/triage-factory/commit/75b90508f82cc2dbc3ed4862032048605aa4ff87))
* **db:** handler-spawned cleanup — WithoutCancel + WithTx ([#193](https://github.com/sky-ai-eng/triage-factory/issues/193)) ([86d262a](https://github.com/sky-ai-eng/triage-factory/commit/86d262a4350c19935f7eda9282e56c15eeb0f737))
* **db:** host-scoped GitHub identity bindings (SKY-396) ([#273](https://github.com/sky-ai-eng/triage-factory/issues/273)) ([0583a9e](https://github.com/sky-ai-eng/triage-factory/commit/0583a9e139d3352fbe2b5ad0f50c7d41068c8622))
* **db:** lift events into per-backend EventStore interface ([#186](https://github.com/sky-ai-eng/triage-factory/issues/186)) ([4779ba8](https://github.com/sky-ai-eng/triage-factory/commit/4779ba8c60bd4ba5220ab04bef16758e9cffb5ff))
* **db:** move GitHub App identity to org_github_apps (SKY-348) ([#235](https://github.com/sky-ai-eng/triage-factory/issues/235)) ([045308a](https://github.com/sky-ai-eng/triage-factory/commit/045308af2134fd5cd2177f95cbe9de37b9013fb0))
* **db:** move model & auto-delegate to team_settings (SKY-354) ([#236](https://github.com/sky-ai-eng/triage-factory/issues/236)) ([ac734d0](https://github.com/sky-ai-eng/triage-factory/commit/ac734d029c1d8acd79e143d29c85b05c881cba0b))
* **db:** per-team GitHub repo tracking + router team↔repo gate (SKY-375) ([#260](https://github.com/sky-ai-eng/triage-factory/issues/260)) ([e9a6e92](https://github.com/sky-ai-eng/triage-factory/commit/e9a6e92d679f9cb64210013cdcfa760b916f1cab))
* **db:** pre-v1.11.0 baseline freeze (TFAC-320) ([#390](https://github.com/sky-ai-eng/triage-factory/issues/390)) ([087041e](https://github.com/sky-ai-eng/triage-factory/commit/087041e457c76c5dcb7be9184d3574295ad54bd3))
* **db:** refactor TaskMemoryStore with dual-pool architecture ([#188](https://github.com/sky-ai-eng/triage-factory/issues/188)) ([914cf8a](https://github.com/sky-ai-eng/triage-factory/commit/914cf8a3ae8a61cc6dcdb09c57d1a8905de6e612))
* **db:** server startup + session middleware - admin pool routing ([#192](https://github.com/sky-ai-eng/triage-factory/issues/192)) ([050b9e4](https://github.com/sky-ai-eng/triage-factory/commit/050b9e426369b97ab65c81dbc8a72428b1478c7c))
* **db:** SKY-246 D2 — Swipe + Dashboard + Secret stores ([#144](https://github.com/sky-ai-eng/triage-factory/issues/144)) ([594e445](https://github.com/sky-ai-eng/triage-factory/commit/594e44513ed649d078fcdf1a8ed3a84e07df16b8))
* **db:** SKY-246 D2 — TaskRuleStore for both backends ([#146](https://github.com/sky-ai-eng/triage-factory/issues/146)) ([4e007e3](https://github.com/sky-ai-eng/triage-factory/commit/4e007e3a53d2773732983700c61acafd96030a68))
* **db:** SKY-246 D2 — TriggerStore for both backends ([#148](https://github.com/sky-ai-eng/triage-factory/issues/148)) ([a67e256](https://github.com/sky-ai-eng/triage-factory/commit/a67e2560fba61e54196bb6a49ebfec586c980ac5))
* **db:** SKY-246 D2 wave 0 + ScoreStore pilot ([#142](https://github.com/sky-ai-eng/triage-factory/issues/142)) ([c4f3c2b](https://github.com/sky-ai-eng/triage-factory/commit/c4f3c2b5aa32e63e0d6eb18709ef4bc98102542e))
* **db:** SKY-246 D2 wave 1 — PromptStore for both backends ([#143](https://github.com/sky-ai-eng/triage-factory/issues/143)) ([6ad0c18](https://github.com/sky-ai-eng/triage-factory/commit/6ad0c18602a1d57cf0b04491a1f231e9ec1442c4))
* **db:** SKY-247 D3 multi-tenant Postgres baseline ([#141](https://github.com/sky-ai-eng/triage-factory/issues/141)) ([9795b7b](https://github.com/sky-ai-eng/triage-factory/commit/9795b7bd4d9bfbc1797197344d17e5e96195751a))
* **db:** SKY-259 — unify task_rules + prompt_triggers into event_handlers ([#156](https://github.com/sky-ai-eng/triage-factory/issues/156)) ([99df393](https://github.com/sky-ai-eng/triage-factory/commit/99df39394f8d19b544b0ad2b49445c5eb2549557))
* **db:** SKY-260 — agents + team_agents stores + bootstrap ([#150](https://github.com/sky-ai-eng/triage-factory/issues/150)) ([d7128d4](https://github.com/sky-ai-eng/triage-factory/commit/d7128d46e7904fe426653fabd717ab0c830a0252))
* **db:** SKY-261 — claim flow on tasks + runs ([#159](https://github.com/sky-ai-eng/triage-factory/issues/159)) ([490c794](https://github.com/sky-ai-eng/triage-factory/commit/490c7946c85a077c726a145aa4f61251b77ecda7))
* **db:** SKY-269 — synthetic tenancy rows in SQLite ([#153](https://github.com/sky-ai-eng/triage-factory/issues/153)) ([268562e](https://github.com/sky-ai-eng/triage-factory/commit/268562e428bd193b6d4cdb39ff43f68356ca425c))
* **db:** SKY-283 – TaskStore (SQLite + Postgres) ([#174](https://github.com/sky-ai-eng/triage-factory/issues/174)) ([718fef1](https://github.com/sky-ai-eng/triage-factory/commit/718fef1f6235e0d4e6884242c5bab7ea9a5daf30))
* **db:** SKY-284 – EntityStore for both backends ([#177](https://github.com/sky-ai-eng/triage-factory/issues/177)) ([10b59ce](https://github.com/sky-ai-eng/triage-factory/commit/10b59cead3d46b5bb1739b76fc58ae8473330584))
* **db:** SKY-285 – AgentRunStore for both backends ([#176](https://github.com/sky-ai-eng/triage-factory/issues/176)) ([b82d02d](https://github.com/sky-ai-eng/triage-factory/commit/b82d02d9fe8fd0bd8b32e7c2d65d1634f57eeea1))
* **db:** SKY-286 – ReviewStore for both backends ([#178](https://github.com/sky-ai-eng/triage-factory/issues/178)) ([78b0c6f](https://github.com/sky-ai-eng/triage-factory/commit/78b0c6f057dab899c3d6f9b5eab910638284ba9e))
* **db:** SKY-287 – PendingPRStore for both backends ([#179](https://github.com/sky-ai-eng/triage-factory/issues/179)) ([565222e](https://github.com/sky-ai-eng/triage-factory/commit/565222eb42a617be5c54434be152a852895af826))
* **db:** SKY-288 – RepoStore for both backends ([#180](https://github.com/sky-ai-eng/triage-factory/issues/180)) ([92f60f3](https://github.com/sky-ai-eng/triage-factory/commit/92f60f39664d6f0e6d07edde20d4166613a53ddf))
* **db:** SKY-289 — PendingFiringsStore for both backends ([#181](https://github.com/sky-ai-eng/triage-factory/issues/181)) ([c928e52](https://github.com/sky-ai-eng/triage-factory/commit/c928e527f82449d079a7cf3c06ad97adb7ff0631))
* **db:** SKY-290 – ProjectStore for both backends ([#182](https://github.com/sky-ai-eng/triage-factory/issues/182)) ([16f13b2](https://github.com/sky-ai-eng/triage-factory/commit/16f13b266850ce2209854017d6a616f9702ec33f))
* **db:** SKY-292 – FactoryReadStore for both backends ([#175](https://github.com/sky-ai-eng/triage-factory/issues/175)) ([f98303b](https://github.com/sky-ai-eng/triage-factory/commit/f98303b407c823b55d52768bd563cac3a6dae293))
* **db:** standardize secrets management (SKY-249) ([#210](https://github.com/sky-ai-eng/triage-factory/issues/210)) ([aa693a3](https://github.com/sky-ai-eng/triage-factory/commit/aa693a331e1cda530676ccc98823ea7379d0c50c))
* **db:** stores for orgs, teams, users, & Jira status (SKY-356) ([#239](https://github.com/sky-ai-eng/triage-factory/issues/239)) ([db53cc4](https://github.com/sky-ai-eng/triage-factory/commit/db53cc4e7c569651fdcd2358847d7f5be5a46ea6))
* **db:** v1.11.0 hard cutover — brick pre-v1.11.0 installs, collapse migration tree ([#162](https://github.com/sky-ai-eng/triage-factory/issues/162)) ([6744853](https://github.com/sky-ai-eng/triage-factory/commit/67448539fb403006c8d356ddfc4fa3c433af3590))
* **delegate:** gzip workspace snapshot blobs (TFAC-332) ([#388](https://github.com/sky-ai-eng/triage-factory/issues/388)) ([d3d90ea](https://github.com/sky-ai-eng/triage-factory/commit/d3d90ea130fff60d1ff543b547c84110af7ece71))
* **delegate:** live runs off the dispatcher with hibernate-on-idle (TFAC-305) ([#351](https://github.com/sky-ai-eng/triage-factory/issues/351)) ([d2bc288](https://github.com/sky-ai-eng/triage-factory/commit/d2bc288664e0bc12bd0e7afc0da431a89208038b))
* **delegate:** message/interrupt + WS permissions (TFAC-309) ([#361](https://github.com/sky-ai-eng/triage-factory/issues/361)) ([eddd9e0](https://github.com/sky-ai-eng/triage-factory/commit/eddd9e01ba248f81f73121480263bcb1ea75c1e1))
* **delegate:** remove yield, keep re-prompt (TFAC-310) ([#357](https://github.com/sky-ai-eng/triage-factory/issues/357)) ([e6f6d68](https://github.com/sky-ai-eng/triage-factory/commit/e6f6d68d1d7d5251b62a39facc72ad096883935e))
* **delegate:** resume semantics for aborted + pending_approval runs (TFAC-319) ([#389](https://github.com/sky-ai-eng/triage-factory/issues/389)) ([a2b2a27](https://github.com/sky-ai-eng/triage-factory/commit/a2b2a27b6abd5bdee2cdbf6810b75021c094ee7e))
* **delegate:** snapshot workspaces for run resumption (SKY-423) ([#296](https://github.com/sky-ai-eng/triage-factory/issues/296)) ([2814fe6](https://github.com/sky-ai-eng/triage-factory/commit/2814fe6099de510b75433c9971957248c5c0de36))
* **delegate:** synthetic-claims for manual runs, admin pool for event-triggered ([#191](https://github.com/sky-ai-eng/triage-factory/issues/191)) ([7fa095e](https://github.com/sky-ai-eng/triage-factory/commit/7fa095ed29483556a8eafb39528ae378051e2517))
* **factory:** cinematic mode (SKY-210) ([#323](https://github.com/sky-ai-eng/triage-factory/issues/323)) ([b02c7d0](https://github.com/sky-ai-eng/triage-factory/commit/b02c7d0176709b4e7880831141f4de4c11e9eebd))
* **factory:** cinematic mode polish ([#331](https://github.com/sky-ai-eng/triage-factory/issues/331)) ([f7cbe1f](https://github.com/sky-ai-eng/triage-factory/commit/f7cbe1f6031479fec96d128ae6c301c56ea610bb))
* **frontend:** SKY-252 – multi-mode auth integration ([#173](https://github.com/sky-ai-eng/triage-factory/issues/173)) ([4929256](https://github.com/sky-ai-eng/triage-factory/commit/492925660408e94581326de58fe2ca64ea9f5ade))
* **gh:** severity badges on PR review comments (SKY-374) ([#257](https://github.com/sky-ai-eng/triage-factory/issues/257)) ([84c7c58](https://github.com/sky-ai-eng/triage-factory/commit/84c7c58bc577b629cd4a057f2b4028babac3f0a1))
* **github:** add GitHub App manifest registration flow (SKY-349) ([#241](https://github.com/sky-ai-eng/triage-factory/issues/241)) ([7714846](https://github.com/sky-ai-eng/triage-factory/commit/7714846242567a28a08b57fda9a827323fe0722d))
* **github:** add GitHub App webhook receiver (SKY-351) ([#251](https://github.com/sky-ai-eng/triage-factory/issues/251)) ([03c9f12](https://github.com/sky-ai-eng/triage-factory/commit/03c9f123853f1ca7333cb9b4aaf90148bc149531))
* **github:** app-aware repo discovery (SKY-365) ([#304](https://github.com/sky-ai-eng/triage-factory/issues/304)) ([e00a22a](https://github.com/sky-ai-eng/triage-factory/commit/e00a22a1d86ca19d7d43a918c69fa3c44c3f17c7))
* **github:** bring-your-own GitHub App import (TFAC-330) ([#382](https://github.com/sky-ai-eng/triage-factory/issues/382)) ([edf1cb7](https://github.com/sky-ai-eng/triage-factory/commit/edf1cb7a681496b3af5c0d8b5043d02eede63076))
* **github:** either/or access; PAT-App switching (TFAC-328) ([#380](https://github.com/sky-ai-eng/triage-factory/issues/380)) ([62db43f](https://github.com/sky-ai-eng/triage-factory/commit/62db43fc0a5259bf5dbb9246f5f82c00a8c71dce))
* **github:** GitHub App registration UI (SKY-350) ([#246](https://github.com/sky-ai-eng/triage-factory/issues/246)) ([7449bda](https://github.com/sky-ai-eng/triage-factory/commit/7449bdae8218420c5675da5efed4c0cd0f780ecc))
* **github:** GitHub Connect OAuth flow for GH identity (SKY-271) ([#274](https://github.com/sky-ai-eng/triage-factory/issues/274)) ([21c9d54](https://github.com/sky-ai-eng/triage-factory/commit/21c9d54ef7feeb77614bb41622e69e29b66ebf59))
* **github:** GitHub credential resolver (SKY-352) ([#252](https://github.com/sky-ai-eng/triage-factory/issues/252)) ([7cf2ae6](https://github.com/sky-ai-eng/triage-factory/commit/7cf2ae6abb1a28b5abbae4a331a5cb6a77c3137f))
* **github:** persist GitHub App owner type to seed UI (TFAC-325) ([#375](https://github.com/sky-ai-eng/triage-factory/issues/375)) ([e817686](https://github.com/sky-ai-eng/triage-factory/commit/e8176869d60c6bfdf72feafc7a08e0da57538e08))
* **github:** REST enumeration, conditionals, apps (SKY-353) ([#259](https://github.com/sky-ai-eng/triage-factory/issues/259)) ([b515dcb](https://github.com/sky-ai-eng/triage-factory/commit/b515dcb31d9803add1030f03d16a6b6dcf955e43))
* **gitproxy:** per-run GitHub App token proxy for git (SKY-333) ([#221](https://github.com/sky-ai-eng/triage-factory/issues/221)) ([c25d97d](https://github.com/sky-ai-eng/triage-factory/commit/c25d97d63b701d00da11b7d21f154d89edb017df))
* **jira:** add team-scoped Jira discovery read (SKY-367) ([#255](https://github.com/sky-ai-eng/triage-factory/issues/255)) ([87f6f59](https://github.com/sky-ai-eng/triage-factory/commit/87f6f596f91107a56591f115351ecb4ce148d3f0))
* **jira:** host-scoped table + store (TFAC-66) ([#372](https://github.com/sky-ai-eng/triage-factory/issues/372)) ([fcf66a5](https://github.com/sky-ai-eng/triage-factory/commit/fcf66a5fccb496b0568ca0a6c37f00d7b311cd7a))
* **jira:** mirror board lifecycle back to Jira (TFAC-300) ([#343](https://github.com/sky-ai-eng/triage-factory/issues/343)) ([0f26c23](https://github.com/sky-ai-eng/triage-factory/commit/0f26c23f7086ccd510f76e533f330770102c19c6))
* **jira:** per-project Jira status rules configuration ([#165](https://github.com/sky-ai-eng/triage-factory/issues/165)) ([9e7c9df](https://github.com/sky-ai-eng/triage-factory/commit/9e7c9dfd6866fcee25e7679755c98ca0317c8eff))
* **jira:** pluggable auth + API version (SKY-441) ([#312](https://github.com/sky-ai-eng/triage-factory/issues/312)) ([52669bb](https://github.com/sky-ai-eng/triage-factory/commit/52669bb858a0e75d9946787d3f9578b53a374d13))
* **jira:** scope Jira team&lt;-&gt;project on router&poller (SKY-376) ([#261](https://github.com/sky-ai-eng/triage-factory/issues/261)) ([8f98b3a](https://github.com/sky-ai-eng/triage-factory/commit/8f98b3a69046e60f31957fe4c747657968ecc97e))
* **jira:** write-actor resolver routes writes by provenance (TFAC-34) ([#341](https://github.com/sky-ai-eng/triage-factory/issues/341)) ([f111c21](https://github.com/sky-ai-eng/triage-factory/commit/f111c213a3089fbf114ffde8dbd82b06ed2ee8ec))
* **llmproxy:** per-run credential proxy (SKY-331) ([#214](https://github.com/sky-ai-eng/triage-factory/issues/214)) ([0a4c365](https://github.com/sky-ai-eng/triage-factory/commit/0a4c3651e8d88093c807d315d8fc29ae1e4d99cb))
* **onboarding:** atomic setup-wizard steps + hide road ahead (SKY-456) ([#332](https://github.com/sky-ai-eng/triage-factory/issues/332)) ([95c5ad5](https://github.com/sky-ai-eng/triage-factory/commit/95c5ad5b616fb1726458e68d4ce6bc8b6b20a659))
* **onboarding:** don't provision at boot in local mode (SKY-436) ([#307](https://github.com/sky-ai-eng/triage-factory/issues/307)) ([067cb2a](https://github.com/sky-ai-eng/triage-factory/commit/067cb2a478876a9cbdb2ae858f185119d03360b8))
* **onboarding:** gate the product on setup state (SKY-444) ([#318](https://github.com/sky-ai-eng/triage-factory/issues/318)) ([74ff52f](https://github.com/sky-ai-eng/triage-factory/commit/74ff52fae4a26d21ce808abd8e738847c3d06264))
* **onboarding:** Jira gate (RequireJiraIdentity) + untangle org access from user identity (TFAC-1) ([#340](https://github.com/sky-ai-eng/triage-factory/issues/340)) ([0967f07](https://github.com/sky-ai-eng/triage-factory/commit/0967f07b0cb47d452e301da1019acf7e33fe5f2e))
* **onboarding:** liquid-glass setup flow overhaul (SKY-457) ([#333](https://github.com/sky-ai-eng/triage-factory/issues/333)) ([0a0d1e3](https://github.com/sky-ai-eng/triage-factory/commit/0a0d1e3082f377f5f34ba97ece1515116b12f257))
* **onboarding:** local first-run via shared config (SKY-443) ([#313](https://github.com/sky-ai-eng/triage-factory/issues/313)) ([f9c0d1a](https://github.com/sky-ai-eng/triage-factory/commit/f9c0d1a2dc032f1187a692d118c82c6004342918))
* **onboarding:** org-create backend + wire "Start your Factory" CTA (SKY-438) ([d97b8f1](https://github.com/sky-ai-eng/triage-factory/commit/d97b8f1a66887eb3b3a8e0043a4b2f133fb2e8fe))
* **onboarding:** per-user GitHub identity (SKY-458) ([#336](https://github.com/sky-ai-eng/triage-factory/issues/336)) ([c420462](https://github.com/sky-ai-eng/triage-factory/commit/c42046267e634672c3ee726dc3ab82edb23ebf64))
* **onboarding:** per-user Jira access capture + store (SKY-461) ([#338](https://github.com/sky-ai-eng/triage-factory/issues/338)) ([1bf28a7](https://github.com/sky-ai-eng/triage-factory/commit/1bf28a7a589f7785711466f524a9e8510d2e3382))
* **onboarding:** retire OrgConfigure/TeamConfigure (SKY-454) ([#329](https://github.com/sky-ai-eng/triage-factory/issues/329)) ([4314d19](https://github.com/sky-ai-eng/triage-factory/commit/4314d19c64b7805978f1eff3e1231062d77d02a6))
* **onboarding:** retire silent signup provisioning (SKY-437) ([#308](https://github.com/sky-ai-eng/triage-factory/issues/308)) ([18c18d7](https://github.com/sky-ai-eng/triage-factory/commit/18c18d76d19d6f1b458215d74dbe0c9238579c32))
* **onboarding:** setup wizard organization steps (SKY-448) ([#325](https://github.com/sky-ai-eng/triage-factory/issues/325)) ([3fe04a8](https://github.com/sky-ai-eng/triage-factory/commit/3fe04a84bc2661981da1712a1d5d8e9c4f99f684))
* **onboarding:** setup wizard shell (SKY-446) ([#320](https://github.com/sky-ai-eng/triage-factory/issues/320)) ([aa06877](https://github.com/sky-ai-eng/triage-factory/commit/aa068774d7cec3a23a3968494c959d85e147f13e))
* **onboarding:** setup wizard team steps (SKY-449) ([#327](https://github.com/sky-ai-eng/triage-factory/issues/327)) ([b8181e6](https://github.com/sky-ai-eng/triage-factory/commit/b8181e6a41bb4e92281871c366615cfa96a22af0))
* **onboarding:** shared team-config + team onboarding (SKY-440) ([#311](https://github.com/sky-ai-eng/triage-factory/issues/311)) ([80bd150](https://github.com/sky-ai-eng/triage-factory/commit/80bd15006de364ea282f4cdeebc120047d367de1))
* **onboarding:** URL-only reachability checks for URLs (SKY-447) ([#321](https://github.com/sky-ai-eng/triage-factory/issues/321)) ([fd07164](https://github.com/sky-ai-eng/triage-factory/commit/fd071640d81d537092bde0ab3613f65f5b928134))
* **orgs:** add org-template editor for seeding teams (SKY-381) ([#268](https://github.com/sky-ai-eng/triage-factory/issues/268)) ([a5db946](https://github.com/sky-ai-eng/triage-factory/commit/a5db9467da8e3e8dc0fff3fb522bd60e0665c397))
* **orgs:** client-side name maxLength + 400-path tests ([57e7f1e](https://github.com/sky-ai-eng/triage-factory/commit/57e7f1e0696705e983bd2c65d42fcc4f25f2aac6))
* **paths:** mode/org-aware state-root resolvers + caller sweep (SKY-408) ([#278](https://github.com/sky-ai-eng/triage-factory/issues/278)) ([de85643](https://github.com/sky-ai-eng/triage-factory/commit/de85643914b01a747f9dd2cbfb1ffd622e931d97))
* **poller:** add per-org polling cadence (SKY-386) ([#267](https://github.com/sky-ai-eng/triage-factory/issues/267)) ([5e371ee](https://github.com/sky-ai-eng/triage-factory/commit/5e371ee63607751f68ab5362492d35106df320d5))
* **poller:** iterate orgs in background services (SKY-312) ([#196](https://github.com/sky-ai-eng/triage-factory/issues/196)) ([92e4188](https://github.com/sky-ai-eng/triage-factory/commit/92e4188a840ef83f5c7166faeb67bc2f735138a1))
* **poller:** route review_requested tasks by reviewer (SKY-370) ([#277](https://github.com/sky-ai-eng/triage-factory/issues/277)) ([31465da](https://github.com/sky-ai-eng/triage-factory/commit/31465da8558fbab5fff7abaf9b60f698d82d78b8))
* **predicate:** Jira *_is_self → *_in allowlist cutover ([#166](https://github.com/sky-ai-eng/triage-factory/issues/166)) ([069bc44](https://github.com/sky-ai-eng/triage-factory/commit/069bc443186523b34a2c19360d11a75bb7d8bb85))
* **predicate:** SKY-264 — author_in / reviewer_in / commenter_in ([#161](https://github.com/sky-ai-eng/triage-factory/issues/161)) ([f659d63](https://github.com/sky-ai-eng/triage-factory/commit/f659d63cbc796195acd4b8366876054a23c6651c))
* **prompts:** decouple canvas layout from step order (TFAC-312) ([#362](https://github.com/sky-ai-eng/triage-factory/issues/362)) ([9b3e1e5](https://github.com/sky-ai-eng/triage-factory/commit/9b3e1e58fecb954477bf8764afbc804ba6559478))
* **prompts:** per-prompt model override ([#149](https://github.com/sky-ai-eng/triage-factory/issues/149)) ([608b48e](https://github.com/sky-ai-eng/triage-factory/commit/608b48e591e9e65d8dd200b79653462919602b11))
* **review:** split PR review into a 3-step blueprint (TFAC-341) ([#396](https://github.com/sky-ai-eng/triage-factory/issues/396)) ([56efcf4](https://github.com/sky-ai-eng/triage-factory/commit/56efcf4564d9cb1c0d6fbfafa5de025e811f179f))
* **router:** durable DB-backed event queue (SKY-414) ([#283](https://github.com/sky-ai-eng/triage-factory/issues/283)) ([3922497](https://github.com/sky-ai-eng/triage-factory/commit/3922497c27ec8c16645c4830e285c596cdd58378))
* **routing:** assignee-centric Jira event routing (TFAC-321) ([#373](https://github.com/sky-ai-eng/triage-factory/issues/373)) ([ba7b6a9](https://github.com/sky-ai-eng/triage-factory/commit/ba7b6a9b75722b9600b9151ec98a645f1c8a4250))
* **routing:** tasks-per-team, remove tracker shortcut ([#183](https://github.com/sky-ai-eng/triage-factory/issues/183)) ([29f5c25](https://github.com/sky-ai-eng/triage-factory/commit/29f5c2576210bf63ed34d9d419ec8dcf3acb2c1d))
* **runmode:** mode flag infrastructure (SKY-248 D4a) ([#139](https://github.com/sky-ai-eng/triage-factory/issues/139)) ([237c524](https://github.com/sky-ai-eng/triage-factory/commit/237c5247a43b718c4ad2037e79500fba7ecef377))
* **sandbox:** agenthost IPC for agent creds isolation (SKY-303) ([#222](https://github.com/sky-ai-eng/triage-factory/issues/222)) ([3ba21e8](https://github.com/sky-ai-eng/triage-factory/commit/3ba21e8a876caaf98fed48c588d7c104ff7d9832))
* **sandbox:** build agent toolchain into alpine rootfs (SKY-337) ([#217](https://github.com/sky-ai-eng/triage-factory/issues/217)) ([0cd123d](https://github.com/sky-ai-eng/triage-factory/commit/0cd123d203ad75fc0df22a1f1a77f01fa594274f))
* **sandbox:** cross-tenant isolation (SKY-395) ([#270](https://github.com/sky-ai-eng/triage-factory/issues/270)) ([2863dcc](https://github.com/sky-ai-eng/triage-factory/commit/2863dccadb84277d3a403f95e92aab7eea127151))
* **sandbox:** git credential proxy -&gt; agentproc.Run (TFAC-302) ([#356](https://github.com/sky-ai-eng/triage-factory/issues/356)) ([1712087](https://github.com/sky-ai-eng/triage-factory/commit/17120875184965d34d846cab1409536e6adfd661))
* **sandbox:** gVisor wrap for agentproc.Run (SKY-254) ([#216](https://github.com/sky-ai-eng/triage-factory/issues/216)) ([07ba1a5](https://github.com/sky-ai-eng/triage-factory/commit/07ba1a567263c66cf682373ea518269adb5a76dc))
* **sandbox:** route exec gh API calls host-side (TFAC-67) ([#348](https://github.com/sky-ai-eng/triage-factory/issues/348)) ([9d37d8b](https://github.com/sky-ai-eng/triage-factory/commit/9d37d8bbd081b83082d7e26b3f35a214f83edd44))
* **sandbox:** route exec jira API calls host-side via agenthost (TFAC-306) ([#344](https://github.com/sky-ai-eng/triage-factory/issues/344)) ([0109094](https://github.com/sky-ai-eng/triage-factory/commit/0109094c71824a2918d6ce54400064c4cc841177))
* **server:** bind to 127.0.0.1 by default, add --host flag ([#169](https://github.com/sky-ai-eng/triage-factory/issues/169)) ([b542a16](https://github.com/sky-ai-eng/triage-factory/commit/b542a16e6e21b72700e33592024f24dbcfd8f3b6))
* **server:** on-demand GitHub App installation refresh endpoint (TFAC-324) ([#376](https://github.com/sky-ai-eng/triage-factory/issues/376)) ([78d7222](https://github.com/sky-ai-eng/triage-factory/commit/78d72223980f43c9a892e8c4e73ddb636ca5cf9a))
* **settings:** EffectiveModel helper — org max-tier cap over team default ([5c6cf51](https://github.com/sky-ai-eng/triage-factory/commit/5c6cf515f01237d02ff0ad3587679ce7a049b1e5))
* **settings:** extract shared org-config field groups; compose in Settings, create-configure, Setup (SKY-439) ([#310](https://github.com/sky-ai-eng/triage-factory/issues/310)) ([3edc08f](https://github.com/sky-ai-eng/triage-factory/commit/3edc08f1eb172b219e383b6ab87053df5fe53309))
* **settings:** redesign to non-progressive glass (SKY-459) ([#337](https://github.com/sky-ai-eng/triage-factory/issues/337)) ([14b95e2](https://github.com/sky-ai-eng/triage-factory/commit/14b95e2843381bfe4ad5f0acd3615e88499d0e4b))
* **settings:** split settings into org/team/user (SKY-358) ([#244](https://github.com/sky-ai-eng/triage-factory/issues/244)) ([4b22966](https://github.com/sky-ai-eng/triage-factory/commit/4b2296655b10f3b2ee5a5855c205b85ab226627f))
* **settings:** split settings into user/team/org (SKY-357) ([#243](https://github.com/sky-ai-eng/triage-factory/issues/243)) ([7f6f462](https://github.com/sky-ai-eng/triage-factory/commit/7f6f462016402e89b59f9b491b07f2389f2f3000))
* **setup,settings:** PAT↔App switching UX (TFAC-329) ([#381](https://github.com/sky-ai-eng/triage-factory/issues/381)) ([7376a74](https://github.com/sky-ai-eng/triage-factory/commit/7376a745c2b4f4f73dab141361fe857c7b3a4039))
* **setup:** App install as new step with redirect (TFAC-326) ([#378](https://github.com/sky-ai-eng/triage-factory/issues/378)) ([9e67a4d](https://github.com/sky-ai-eng/triage-factory/commit/9e67a4d9d95a6be18f77cac73493ac595ccf7fd4))
* **setup:** GitHub team-&gt;TF team mapping wizard (SKY-411) ([#281](https://github.com/sky-ai-eng/triage-factory/issues/281)) ([3fb782e](https://github.com/sky-ai-eng/triage-factory/commit/3fb782e517b5c653406a17127b54eea757920aa0))
* **setup:** reuse the org PAT as the user's own identity in local onboarding (TFAC-335) ([#391](https://github.com/sky-ai-eng/triage-factory/issues/391)) ([a72f1ab](https://github.com/sky-ai-eng/triage-factory/commit/a72f1abdcc5d4ef3bcb6eb24d8278f49d3401a62))
* **setup:** unify GitHub-team candidate source across wizard + Settings (SKY-413) ([#282](https://github.com/sky-ai-eng/triage-factory/issues/282)) ([6930130](https://github.com/sky-ai-eng/triage-factory/commit/69301308cbacdf36666e907ce42d7b85e091ee5c))
* **tasks:** reverse per-team fan-out (SKY-368) ([#256](https://github.com/sky-ai-eng/triage-factory/issues/256)) ([e30aa42](https://github.com/sky-ai-eng/triage-factory/commit/e30aa42c9ad9d440a3d02a9dabb527581c3465c5))
* **teams:** make GitHub team checklist reusable (SKY-388) ([#294](https://github.com/sky-ai-eng/triage-factory/issues/294)) ([10485bb](https://github.com/sky-ai-eng/triage-factory/commit/10485bb7a7899ba6a806ecadd89ee8c61c5a26a0))
* **teams:** multi-team read filter + write picker + new-team/org bootstrap (SKY-378) ([#263](https://github.com/sky-ai-eng/triage-factory/issues/263)) ([2cdf180](https://github.com/sky-ai-eng/triage-factory/commit/2cdf180ceb91a2b312eaf1beec38f2fd8ec9e17a))
* **teams:** prompts and event handlers now team-scoped (SKY-380) ([#266](https://github.com/sky-ai-eng/triage-factory/issues/266)) ([db45d5d](https://github.com/sky-ai-eng/triage-factory/commit/db45d5d96c51e245183ff5abd45423ac29620bd7))
* **teams:** thread acting team through Create handlers (SKY-377) ([#262](https://github.com/sky-ai-eng/triage-factory/issues/262)) ([6da7f41](https://github.com/sky-ai-eng/triage-factory/commit/6da7f41ae11a941791b315cb05855649c444a590))
* **ui:** add light/dark/auto theme with flat-surface palette ([#160](https://github.com/sky-ai-eng/triage-factory/issues/160)) ([6cdc105](https://github.com/sky-ai-eng/triage-factory/commit/6cdc105496cf52bbaf7f846509969c150b47f201))
* **ui:** redesign fullscreen agent run view (TFAC-314) ([#366](https://github.com/sky-ai-eng/triage-factory/issues/366)) ([204558d](https://github.com/sky-ai-eng/triage-factory/commit/204558deca7d71164cba565cb3b8690190c0e1af))
* **ui:** run composer, interrupt, permissions, context meter (TFAC-315) ([#367](https://github.com/sky-ai-eng/triage-factory/issues/367)) ([7e4a814](https://github.com/sky-ai-eng/triage-factory/commit/7e4a8148bf6f09af3a40150c2ef5c7775a47923e))
* **ui:** show id badge on review_requested tasks (SKY-412) ([#280](https://github.com/sky-ai-eng/triage-factory/issues/280)) ([fe3fce0](https://github.com/sky-ai-eng/triage-factory/commit/fe3fce056cf3ef601d4d040152f2d73c9db57dfc))
* **worktree:** inject App token into host-side clone; hardwire HTTPS in multi-mode (SKY-391) ([#275](https://github.com/sky-ai-eng/triage-factory/issues/275)) ([3dbf878](https://github.com/sky-ai-eng/triage-factory/commit/3dbf878847dfb97adbd40892cd9926705f33c313))
* **worktree:** seed-on-demand + evictable bare cache (TFAC-60) ([#383](https://github.com/sky-ai-eng/triage-factory/issues/383)) ([c06edba](https://github.com/sky-ai-eng/triage-factory/commit/c06edbab1833dab15ec1e6866353858ed6fe8dbf))


### Bug Fixes

* address Copilot review comments ([3eba6fc](https://github.com/sky-ai-eng/triage-factory/commit/3eba6fc9664914c6a1e641dc13ef5cc9c3708eeb))
* **agentproc:** handle latent agent sdk bug (TFAC-317) ([#368](https://github.com/sky-ai-eng/triage-factory/issues/368)) ([97dcae3](https://github.com/sky-ai-eng/triage-factory/commit/97dcae380896e447e0dc6d77eda764206a7b0c76))
* **agentproc:** surface Claude Code subprocess stderr for diagnostics ([#218](https://github.com/sky-ai-eng/triage-factory/issues/218)) ([f54429a](https://github.com/sky-ai-eng/triage-factory/commit/f54429ac8ec4b2bc52dbc75e79b6314475de9c0b))
* **auth:** route credentials through SecretStore seam (SKY-329) ([#211](https://github.com/sky-ai-eng/triage-factory/issues/211)) ([c7c2f5f](https://github.com/sky-ai-eng/triage-factory/commit/c7c2f5fb3696b8d17cc1098526c3343dcbb2fc4b))
* **auth:** send JSON body to gotrue /token ([#231](https://github.com/sky-ai-eng/triage-factory/issues/231)) ([726640c](https://github.com/sky-ai-eng/triage-factory/commit/726640cd3f768e4a86302174e0195c727a12f0a0))
* **blueprint:** render blueprint steps as canvas nodes (SKY-428) ([#299](https://github.com/sky-ai-eng/triage-factory/issues/299)) ([fdb04c8](https://github.com/sky-ai-eng/triage-factory/commit/fdb04c800ab416d513622e6be0f1665fa521c6ea))
* **blueprint:** replace chain CLI with runs.outcome (SKY-419) ([#288](https://github.com/sky-ai-eng/triage-factory/issues/288)) ([3c855f3](https://github.com/sky-ai-eng/triage-factory/commit/3c855f3a55020e8120cc81e5e868c29d297c3b63))
* **blueprint:** show frozen steps on board (TFAC-313) ([#363](https://github.com/sky-ai-eng/triage-factory/issues/363)) ([4f87eae](https://github.com/sky-ai-eng/triage-factory/commit/4f87eae5ceb9d3b0e3669c213379d0b64351ce26))
* **boot:** gate local-only startup blocks behind runmode.ModeLocal ([04de37b](https://github.com/sky-ai-eng/triage-factory/commit/04de37b27b76e4e8e5e6c4329fff1946b3badf03))
* **bootstrap:** use admin pool for agent lookup (SKY-385) ([#265](https://github.com/sky-ai-eng/triage-factory/issues/265)) ([2a04b2f](https://github.com/sky-ai-eng/triage-factory/commit/2a04b2f7f85d750c95fa0046fc5c5429d2e86876))
* **compose:** two multi-mode boot fixes + OAuth smoke doc ([#230](https://github.com/sky-ai-eng/triage-factory/issues/230)) ([93a77d8](https://github.com/sky-ai-eng/triage-factory/commit/93a77d871bd3c5f9c22a2bf60985092754dbd7ff))
* **creds:** gofmt classifier_test + refresh retired-UpdateCredentials comments (SKY-389) ([6b0b463](https://github.com/sky-ai-eng/triage-factory/commit/6b0b4633dc0831ef8479a0750e01a48f48d023fc))
* **curator:** route cancel/cleanup through admin-pool (TFAC-64) ([#384](https://github.com/sky-ai-eng/triage-factory/issues/384)) ([51f6f25](https://github.com/sky-ai-eng/triage-factory/commit/51f6f25623cc8957ca2b8b7aa4fa508c11b6ea8b))
* **db:** consolidate into CuratorStore interface (SKY-338) ([#225](https://github.com/sky-ai-eng/triage-factory/issues/225)) ([7c57c75](https://github.com/sky-ai-eng/triage-factory/commit/7c57c75e9082e5b89de60a36519f12b630a2ce2f))
* **db:** detect partial-legacy install state (SKY-245 hotfix) ([#140](https://github.com/sky-ai-eng/triage-factory/issues/140)) ([8961968](https://github.com/sky-ai-eng/triage-factory/commit/8961968556d760786240ca2ef3d3c3e9c30618e6))
* **db:** LifetimeDistinct + curator orphan sweep (SKY-341) ([#228](https://github.com/sky-ai-eng/triage-factory/issues/228)) ([9d80c4d](https://github.com/sky-ai-eng/triage-factory/commit/9d80c4da12978e797ae04fab47ec61911eaa19fc))
* **db:** tighten SQLite to match PG, normalize settings ([#163](https://github.com/sky-ai-eng/triage-factory/issues/163)) ([68f3114](https://github.com/sky-ai-eng/triage-factory/commit/68f3114c927ccf48dc2000b4c747a787e6de4c8c))
* **db:** unblock main after PR [#149](https://github.com/sky-ai-eng/triage-factory/issues/149)/[#150](https://github.com/sky-ai-eng/triage-factory/issues/150) collision ([#151](https://github.com/sky-ai-eng/triage-factory/issues/151)) ([7548863](https://github.com/sky-ai-eng/triage-factory/commit/7548863c82315ce24e1e7130ca04bfa52de9115a))
* **delegate:** deflake permission-resolve stress test on starved runners ([a6e5e24](https://github.com/sky-ai-eng/triage-factory/commit/a6e5e24c230972c225adda588a15c44a1d45c2c0))
* **delegate:** pre-expand run-root paths in agent prompts and tool messages ([#364](https://github.com/sky-ai-eng/triage-factory/issues/364)) ([6cd30bd](https://github.com/sky-ai-eng/triage-factory/commit/6cd30bd771c3ba8465408b2076fb688b46cf23d7))
* **delegate:** RLS-correct routing for manual vs event triggers (SKY-253) ([#200](https://github.com/sky-ai-eng/triage-factory/issues/200)) ([d7aa102](https://github.com/sky-ai-eng/triage-factory/commit/d7aa10216100ff158ddfadc8b43e999d171b9770))
* **domain:** move 12 projection/view types from db/ to domain/ ([#145](https://github.com/sky-ai-eng/triage-factory/issues/145)) ([e76b881](https://github.com/sky-ai-eng/triage-factory/commit/e76b881d870c066cb02a1d07a754c1ecb952c0d0))
* **exec:** silence goose logger to drop "no migrations to run" noise ([#157](https://github.com/sky-ai-eng/triage-factory/issues/157)) ([37b7641](https://github.com/sky-ai-eng/triage-factory/commit/37b7641f6f097193047ebce852e1d06533e2ae3b))
* **frontend:** unbreak `npm run dev` for Babylon scenes ([#154](https://github.com/sky-ai-eng/triage-factory/issues/154)) ([62c495c](https://github.com/sky-ai-eng/triage-factory/commit/62c495c4b6e9e8060cd75f128ff2f51b8020f699))
* **github:** getting github app functional ([#249](https://github.com/sky-ai-eng/triage-factory/issues/249)) ([cd6f5d2](https://github.com/sky-ai-eng/triage-factory/commit/cd6f5d20df271dc74ee0eeef42eba89e62908904))
* **github:** hookless GitHub Apps for local deployments (SKY-362) ([#248](https://github.com/sky-ai-eng/triage-factory/issues/248)) ([37d2c03](https://github.com/sky-ai-eng/triage-factory/commit/37d2c03dc3b159fc029f5cf31a7f6740db955881))
* **github:** move manifest POST to backend (SKY-361) ([#247](https://github.com/sky-ai-eng/triage-factory/issues/247)) ([8e3de95](https://github.com/sky-ai-eng/triage-factory/commit/8e3de95c205483296eb3711ad03c27d08d996afc))
* **github:** worktree ghost leak from cancelled add operations ([#253](https://github.com/sky-ai-eng/triage-factory/issues/253)) ([3da4e6b](https://github.com/sky-ai-eng/triage-factory/commit/3da4e6b9cf38ef6a25c862c8e0881bfd73b2c576))
* **llmproxy:** keep streaming responses alive when upstream replies before the request body is drained ([#374](https://github.com/sky-ai-eng/triage-factory/issues/374)) ([4bb55cc](https://github.com/sky-ai-eng/triage-factory/commit/4bb55cc73099587b24f1361fc0a81219c16ef13e))
* **logs:** remove per-request PAT resolver log line ([#295](https://github.com/sky-ai-eng/triage-factory/issues/295)) ([00f3f92](https://github.com/sky-ai-eng/triage-factory/commit/00f3f92fcf25d3cb9ce7f3874c3541745a8d53af))
* **onboarding:** align Jira PAT UX with GitHub ([#365](https://github.com/sky-ai-eng/triage-factory/issues/365)) ([ccdbd55](https://github.com/sky-ai-eng/triage-factory/commit/ccdbd55c44110311a4c26a90a2481f9777870c40))
* **onboarding:** harden wizard back-nav + summary against omitted steps (SKY-449) ([#328](https://github.com/sky-ai-eng/triage-factory/issues/328)) ([7f9799c](https://github.com/sky-ai-eng/triage-factory/commit/7f9799c390dd0caf5829ed454c20d32712d9f8fe))
* **onboarding:** portal repo picker to body; relabel org step button ([89c4231](https://github.com/sky-ai-eng/triage-factory/commit/89c42315bc6977510f849657e60d82c2e38eba30))
* **orgs:** scope create error handling, cap name length, test local 404 ([5592cf6](https://github.com/sky-ai-eng/triage-factory/commit/5592cf60e64c87250807016cdd1c2d849fc9e4e5))
* **poller:** add fence to prevent duplicate auto-delegations (SKY-424) ([#286](https://github.com/sky-ai-eng/triage-factory/issues/286)) ([a991b2c](https://github.com/sky-ai-eng/triage-factory/commit/a991b2c8d433aca8e06f422c727af9ac703e096a))
* **projects:** classification wait keys on classified_at (SKY-392) ([#269](https://github.com/sky-ai-eng/triage-factory/issues/269)) ([f43cc86](https://github.com/sky-ai-eng/triage-factory/commit/f43cc86923d2df660cf721c9a46ef0221096b9f4))
* **repoprofile:** profile App-only orgs; clean fetch errors (TFAC-331) ([#385](https://github.com/sky-ai-eng/triage-factory/issues/385)) ([b67382d](https://github.com/sky-ai-eng/triage-factory/commit/b67382da85f40acd5ca7a416524269dc203f3498))
* **server:** migrate request handlers off the PAT-only global GitHub client to the resolver (TFAC-327) ([#379](https://github.com/sky-ai-eng/triage-factory/issues/379)) ([f8aee95](https://github.com/sky-ai-eng/triage-factory/commit/f8aee95c6c3087604663903510dac2681b8fc10f))
* **settings:** de-spam cap warnings, share default-model constant, lint ([eba711a](https://github.com/sky-ai-eng/triage-factory/commit/eba711aca6a2ac5a830a4549330b925e5ff8123b))
* **setup:** keep input focus rings from being clipped by the wizard body ([#392](https://github.com/sky-ai-eng/triage-factory/issues/392)) ([a862b22](https://github.com/sky-ai-eng/triage-factory/commit/a862b22ce4ba7878fd49afbaf5da4a8d5aea98d7))
* **tasks:** author-centric task routing for PR events (SKY-372) ([#290](https://github.com/sky-ai-eng/triage-factory/issues/290)) ([24a9b18](https://github.com/sky-ai-eng/triage-factory/commit/24a9b182e865d2da1f09860e684e09b80e5bbf64))
* **tests:** refactor debounce test to use direct fire() (SKY-384) ([#264](https://github.com/sky-ai-eng/triage-factory/issues/264)) ([f3e0ac5](https://github.com/sky-ai-eng/triage-factory/commit/f3e0ac5454ad4e77baa29e8b90da4c6c9cc6efd3))
* **ui:** Use better contrast ([#152](https://github.com/sky-ai-eng/triage-factory/issues/152)) ([4336093](https://github.com/sky-ai-eng/triage-factory/commit/4336093e4cc83e6aabd64c0c23bb52d7c1f1665d))
* **workspace:** validate task entity key before composing feature branch ([#170](https://github.com/sky-ai-eng/triage-factory/issues/170)) ([3471170](https://github.com/sky-ai-eng/triage-factory/commit/347117024654fa281b0ae18d3c0ecec9041e831e))


### Performance Improvements

* **setup:** bound team-repos write validation by selection size, not org size (SKY-409) ([#279](https://github.com/sky-ai-eng/triage-factory/issues/279)) ([66411d0](https://github.com/sky-ai-eng/triage-factory/commit/66411d0173f4cfb5eb5090d1589955e175404870))

## [1.10.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.10.0...v1.10.1) (2026-05-08)


### Bug Fixes

* **board:** surface Return-to-queue on task_unsolvable runs ([#133](https://github.com/sky-ai-eng/triage-factory/issues/133)) ([9bb6272](https://github.com/sky-ai-eng/triage-factory/commit/9bb6272fe0d1cbd3f68b606493253f0a73edace6))

## [1.10.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.9.2...v1.10.0) (2026-05-08)


### Features

* **delegate:** release held takeovers ([#131](https://github.com/sky-ai-eng/triage-factory/issues/131)) ([b2c7316](https://github.com/sky-ai-eng/triage-factory/commit/b2c7316a648bd70b7691cca238c7560da47c4cc9))


### Bug Fixes

* **board:** seed agentRuns on delegate so early run phases are visible ([#130](https://github.com/sky-ai-eng/triage-factory/issues/130)) ([06e511e](https://github.com/sky-ai-eng/triage-factory/commit/06e511ef99d2869e99091a2acf12cf463c6540e1))

## [1.9.2](https://github.com/sky-ai-eng/triage-factory/compare/v1.9.1...v1.9.2) (2026-05-08)


### Bug Fixes

* **delegate:** pnpm/npm script shortcuts + go workspace ops in allowlist ([#128](https://github.com/sky-ai-eng/triage-factory/issues/128)) ([cad3eee](https://github.com/sky-ai-eng/triage-factory/commit/cad3eee9e2ca18e0bcddc156833014f1f5dbcc62))

## [1.9.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.9.0...v1.9.1) (2026-05-08)


### Bug Fixes

* **github:** per-job log fallback when jobs still running ([#127](https://github.com/sky-ai-eng/triage-factory/issues/127)) ([0ebbdef](https://github.com/sky-ai-eng/triage-factory/commit/0ebbdefe28f6a9d6fc79f0086a627cfcded0c462))
* **jira-cli:** make `edit` flag parsing reject ambiguous and trailing flags ([b0edbd1](https://github.com/sky-ai-eng/triage-factory/commit/b0edbd19628d24bca17e65f4f736e50c04685782))

## [1.9.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.8.1...v1.9.0) (2026-05-07)


### Features

* **delegate:** use per-prompt allowed-tools from skills ([#122](https://github.com/sky-ai-eng/triage-factory/issues/122)) ([4c79cf8](https://github.com/sky-ai-eng/triage-factory/commit/4c79cf87cee5dd453f0bc6e9f6a0bdd6783fa1d3))
* **tracker:** add review_request_removed event and reconcile tasks ([#112](https://github.com/sky-ai-eng/triage-factory/issues/112)) ([fef901f](https://github.com/sky-ai-eng/triage-factory/commit/fef901fe75c1738c7bfded7971429cad9c481db4))

## [1.8.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.8.0...v1.8.1) (2026-05-07)


### Bug Fixes

* **backfill:** default all checkboxes off, split sections ([#120](https://github.com/sky-ai-eng/triage-factory/issues/120)) ([acaa696](https://github.com/sky-ai-eng/triage-factory/commit/acaa696210467e0c25b611f42e6cadeca925b638))

## [1.8.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.7.1...v1.8.0) (2026-05-07)


### Features

* **classify:** per-project quorum classifier on entity discovery ([#117](https://github.com/sky-ai-eng/triage-factory/issues/117)) ([342db5c](https://github.com/sky-ai-eng/triage-factory/commit/342db5cd4fb94acceb42c8a4932a5b9358f1df20))
* **delegate:** consolidate scratch dirs + propagate project knowledge ([#116](https://github.com/sky-ai-eng/triage-factory/issues/116)) ([2c1572f](https://github.com/sky-ai-eng/triage-factory/commit/2c1572fbedc68110a809b8f7ab463ee2d188a0a8))
* **projects:** backfill popup on project create + import ([#118](https://github.com/sky-ai-eng/triage-factory/issues/118)) ([c1d30ed](https://github.com/sky-ai-eng/triage-factory/commit/c1d30ede3bdffe7f5a361156dd29976312c42c06))
* **projects:** entities panel under knowledge base on project detail ([#119](https://github.com/sky-ai-eng/triage-factory/issues/119)) ([4b7f615](https://github.com/sky-ai-eng/triage-factory/commit/4b7f6157591d166b62231a94c7ed87c753433bda))


### Bug Fixes

* **uninstall:** drop curator Claude sessions + sync clean-slate.sh ([#114](https://github.com/sky-ai-eng/triage-factory/issues/114)) ([07826bb](https://github.com/sky-ai-eng/triage-factory/commit/07826bb2519083d0f6297bb7c9eca83f76c6f20b))

## [1.7.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.7.0...v1.7.1) (2026-05-06)


### Bug Fixes

* Use creds overlay for settings page ([#110](https://github.com/sky-ai-eng/triage-factory/issues/110)) ([a4eac22](https://github.com/sky-ai-eng/triage-factory/commit/a4eac2239dc1c167c31ebcb211986ded5dce08b3))

## [1.7.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.6.1...v1.7.0) (2026-05-06)


### Features

* **auth:** support TRIAGE_FACTORY_* env vars as credential source ([#105](https://github.com/sky-ai-eng/triage-factory/issues/105)) ([1186013](https://github.com/sky-ai-eng/triage-factory/commit/1186013c0e78649ea8f9a32ab9d0e4dd079cae7d))
* **github:** add SSH/HTTPS toggle for bare-clone setup ([#109](https://github.com/sky-ai-eng/triage-factory/issues/109)) ([873aad3](https://github.com/sky-ai-eng/triage-factory/commit/873aad3455d68d3c2bd3be61e2d8307e66bb7520))


### Performance Improvements

* **test:** cache schema bundle for in-memory test DBs ([#107](https://github.com/sky-ai-eng/triage-factory/issues/107)) ([1fe8696](https://github.com/sky-ai-eng/triage-factory/commit/1fe8696916114fca46419ab79890cfba4ae16a7f))

## [1.6.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.6.0...v1.6.1) (2026-05-06)


### Bug Fixes

* **agentproc:** bufio.Scanner tool_result lines don't kill runs ([#103](https://github.com/sky-ai-eng/triage-factory/issues/103)) ([d6fda3b](https://github.com/sky-ai-eng/triage-factory/commit/d6fda3b3146a62fcfd30227b86a4ba60fb8bd620))
* **config:** settings in SQLite not yaml, default poll to 5m ([#100](https://github.com/sky-ai-eng/triage-factory/issues/100)) ([c65a5b5](https://github.com/sky-ai-eng/triage-factory/commit/c65a5b55489b96728d66a25f8c32250dd9978ce3))
* **factory:** flush turn-pole belt seams via analytic endpoint tangents ([#104](https://github.com/sky-ai-eng/triage-factory/issues/104)) ([9a49369](https://github.com/sky-ai-eng/triage-factory/commit/9a49369ea4e46b07e249737902b167c1832835a8))
* **review:** multi-line comments must be within one diff hunk ([#101](https://github.com/sky-ai-eng/triage-factory/issues/101)) ([96b989d](https://github.com/sky-ai-eng/triage-factory/commit/96b989d38c78ec00b985c6d4e1260a1d4db0a5e3))

## [1.6.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.5.0...v1.6.0) (2026-05-05)


### Features

* **curator:** add built-in Jira formatting skill ([#96](https://github.com/sky-ai-eng/triage-factory/issues/96)) ([31dc26f](https://github.com/sky-ai-eng/triage-factory/commit/31dc26f13579894d12ddfc88d6692258f40c3b38))
* **delegate:** lazy Jira worktrees + pending-PR approval flow ([#98](https://github.com/sky-ai-eng/triage-factory/issues/98)) ([88d75fc](https://github.com/sky-ai-eng/triage-factory/commit/88d75fcdf8d08082cb1f191065ab87c798f271ba))


### Bug Fixes

* **prompts:** add prompt version migration ([#99](https://github.com/sky-ai-eng/triage-factory/issues/99)) ([63f1700](https://github.com/sky-ai-eng/triage-factory/commit/63f1700b5ccfe47d2d7fc860c7603114d866fa04))

## [1.5.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.4.0...v1.5.0) (2026-05-04)


### Features

* **curator:** chat panel + live KB watcher + reset ([#92](https://github.com/sky-ai-eng/triage-factory/issues/92)) ([9bfcbb3](https://github.com/sky-ai-eng/triage-factory/commit/9bfcbb3d1bba2e9aa016228566f50485f47cba14))
* **curator:** envelope + hidden context-change channel ([#90](https://github.com/sky-ai-eng/triage-factory/issues/90)) ([8523521](https://github.com/sky-ai-eng/triage-factory/commit/852352144de78639151ab85a9875a462c42ec5bd))
* **curator:** per-project Claude Code chat sessions ([#87](https://github.com/sky-ai-eng/triage-factory/issues/87)) ([1e9fb2e](https://github.com/sky-ai-eng/triage-factory/commit/1e9fb2e6916e2b7249ad111ad5f5612018595b7d))
* **curator:** per-project ticket-spec skill ([#94](https://github.com/sky-ai-eng/triage-factory/issues/94)) ([ea1e617](https://github.com/sky-ai-eng/triage-factory/commit/ea1e617d5997dd52d67aa68f4148f5b59ff78d82))
* **delegate:** yield-to-user pause/resume for agents (SKY-139) ([#84](https://github.com/sky-ai-eng/triage-factory/issues/84)) ([82e8dc8](https://github.com/sky-ai-eng/triage-factory/commit/82e8dc81680e90bea0705ab92c23eabffb1e9171))
* **projects:** /projects page + tracker links + knowledge sidebar ([#89](https://github.com/sky-ai-eng/triage-factory/issues/89)) ([5f1d4a1](https://github.com/sky-ai-eng/triage-factory/commit/5f1d4a1b3f7a57508986d47e21d81c6372ac4fb0))
* **projects:** add SKY-222 project export/import bundles ([#91](https://github.com/sky-ai-eng/triage-factory/issues/91)) ([25744f5](https://github.com/sky-ai-eng/triage-factory/commit/25744f50c005d164881d9d08b0021b010fd6e05e))
* **projects:** schema + CRUD API (SKY-215) ([#85](https://github.com/sky-ai-eng/triage-factory/issues/85)) ([62bbe02](https://github.com/sky-ai-eng/triage-factory/commit/62bbe020083bc481c138bb697c72584d4b38a120))
* **worktree:** bootstrap bare clones + PR refspec + origin URL repair (SKY-214) ([#82](https://github.com/sky-ai-eng/triage-factory/issues/82)) ([b9e1bd4](https://github.com/sky-ai-eng/triage-factory/commit/b9e1bd4f3290825aa871ee93493c74ba09d3e003))


### Bug Fixes

* **curator:** repo materialization + shared allowlist + git -C support ([#88](https://github.com/sky-ai-eng/triage-factory/issues/88)) ([688785b](https://github.com/sky-ai-eng/triage-factory/commit/688785b3caa60cd86ee0de0f60d99f195138e45a))
* **delegate:** extract agentproc package ([#86](https://github.com/sky-ai-eng/triage-factory/issues/86)) ([e8b3468](https://github.com/sky-ai-eng/triage-factory/commit/e8b346844a6b681241c6d5cc9454092895dd1d46))
* **tracker:** updatedAt-gated PR refresh for monorepo scale ([#93](https://github.com/sky-ai-eng/triage-factory/issues/93)) ([d6f5d2d](https://github.com/sky-ai-eng/triage-factory/commit/d6f5d2d18abeeb3144b26b04f390ea5c301a2483))

## [1.4.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.3.0...v1.4.0) (2026-05-02)


### Features

* **board:** drag AgentCards between columns in terminal run states ([#77](https://github.com/sky-ai-eng/triage-factory/issues/77)) ([71a6d3a](https://github.com/sky-ai-eng/triage-factory/commit/71a6d3aeea74b8e3931c328c94ae113964228009))
* **reviews:** block second submit-review per run + clearer queue wording (SKY-212) ([#78](https://github.com/sky-ai-eng/triage-factory/issues/78)) ([cb02370](https://github.com/sky-ai-eng/triage-factory/commit/cb0237028970ac9b0f428b3b64831e000efd2038))
* **reviews:** cleanup discarded reviews + split /undo from /requeue (SKY-206) ([#75](https://github.com/sky-ai-eng/triage-factory/issues/75)) ([4c5a8b1](https://github.com/sky-ai-eng/triage-factory/commit/4c5a8b1ebafedb1e415f611dc170b63da41e7348))
* **reviews:** persist human verdict to run_memory.human_content (SKY-205) ([#74](https://github.com/sky-ai-eng/triage-factory/issues/74)) ([f5b8512](https://github.com/sky-ai-eng/triage-factory/commit/f5b851279823589a7c858600f1aaaeb6d693a908))
* **reviews:** Return-to-queue button on pending_approval AgentCards (SKY-207) ([#76](https://github.com/sky-ai-eng/triage-factory/issues/76)) ([b522644](https://github.com/sky-ai-eng/triage-factory/commit/b522644dfce370e66b473e0688939730b42ff0fe))


### Bug Fixes

* **delegate:** stamp run-message timestamps before WS broadcast (SKY-213) ([#79](https://github.com/sky-ai-eng/triage-factory/issues/79)) ([1be816c](https://github.com/sky-ai-eng/triage-factory/commit/1be816cb011c79671c03e70164321051e5053139))
* **factory:** drag-to-delegate from station drawer ([#70](https://github.com/sky-ai-eng/triage-factory/issues/70)) ([1ece04d](https://github.com/sky-ai-eng/triage-factory/commit/1ece04d1185fb3e4f34c3edbd92ec43e446b14a5))
* **reviews:** expand pr-review focus + drive inline suggestions ([#81](https://github.com/sky-ai-eng/triage-factory/issues/81)) ([263e846](https://github.com/sky-ai-eng/triage-factory/commit/263e846a1513967ef1fa6aa503d5f020581b48ba))

## [1.3.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.2.1...v1.3.0) (2026-05-01)


### Features

* **factory:** SKY-196 – move to 3d factory view ([#58](https://github.com/sky-ai-eng/triage-factory/issues/58)) ([480d5e6](https://github.com/sky-ai-eng/triage-factory/commit/480d5e6dd0d0b4c43508fec83c761a4977a4b49a))


### Bug Fixes

* **factory:** animate terminal states before closure ([#69](https://github.com/sky-ai-eng/triage-factory/issues/69)) ([07d778c](https://github.com/sky-ai-eng/triage-factory/commit/07d778c5510f5da18f6d548aabd87fa136c9023c))

## [1.2.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.2.0...v1.2.1) (2026-04-30)


### Bug Fixes

* **ci:** set CLA bot required inputs ([489ac92](https://github.com/sky-ai-eng/triage-factory/commit/489ac9247d5742584b2d65e0b4f23be752f24c11))
* **github:** intercept 406 on large PR diffs in start-review ([#64](https://github.com/sky-ai-eng/triage-factory/issues/64)) ([92807ff](https://github.com/sky-ai-eng/triage-factory/commit/92807ff2bcb911a3280a3261084268665a94ec33))
* **tracker:** strip trailing slash from Jira base URL ([#59](https://github.com/sky-ai-eng/triage-factory/issues/59)) ([21394ae](https://github.com/sky-ai-eng/triage-factory/commit/21394ae4ff0ee2be19ed5999ab0fee0a63f30228))

## [1.2.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.1.2...v1.2.0) (2026-04-27)


### Features

* **tracker:** real source times for label / review-request / ready-for-review events ([#56](https://github.com/sky-ai-eng/triage-factory/issues/56)) ([38f1845](https://github.com/sky-ai-eng/triage-factory/commit/38f1845fc2bcc433e67d5a8af0a2957f1c3775e0))

## [1.1.2](https://github.com/sky-ai-eng/triage-factory/compare/v1.1.1...v1.1.2) (2026-04-27)


### Bug Fixes

* **db:** serialize writes so contention queues in Go rather than racing ([#54](https://github.com/sky-ai-eng/triage-factory/issues/54)) ([0da8941](https://github.com/sky-ai-eng/triage-factory/commit/0da894141644debcf1bb93e93cc7324d1af0256a))

## [1.1.1](https://github.com/sky-ai-eng/triage-factory/compare/v1.1.0...v1.1.1) (2026-04-27)


### Bug Fixes

* **deps:** fix npm audit ([d58be7d](https://github.com/sky-ai-eng/triage-factory/commit/d58be7de46cd3aabc482dc1971e11bb253f0befb))

## [1.1.0](https://github.com/sky-ai-eng/triage-factory/compare/v1.0.0...v1.1.0) (2026-04-27)


### Features

* **cmd:** add uninstall command ([#52](https://github.com/sky-ai-eng/triage-factory/issues/52)) ([3128709](https://github.com/sky-ai-eng/triage-factory/commit/3128709a7ccc180ec5319c4606551d27976e6114))


### Bug Fixes

* **ci:** drop component prefix from release-please tags ([#49](https://github.com/sky-ai-eng/triage-factory/issues/49)) ([2ac3bf9](https://github.com/sky-ai-eng/triage-factory/commit/2ac3bf95970a41193c5a6ccc4846fa864fb54af9))
* **prompts:** deduplicate skill search dirs and skills themselves ([#51](https://github.com/sky-ai-eng/triage-factory/issues/51)) ([2832b92](https://github.com/sky-ai-eng/triage-factory/commit/2832b9294c012e1b5da4192ab0d5ea55cfc611bc))

## 1.0.0 (2026-04-27)


### Features

* add codeowners ([92a9e1a](https://github.com/sky-ai-eng/triage-factory/commit/92a9e1a528777a4939805c46c53c3ded1c961bdf))
* add configurable jira tags to watch for ([4e2106d](https://github.com/sky-ai-eng/triage-factory/commit/4e2106d7a33abe8787e60f1769a5a2584c172633))
* **board:** allow agent delegated tasks to move around ([0a32914](https://github.com/sky-ai-eng/triage-factory/commit/0a329141cdd4396b9bb8a40c5cdf00b661a2520e))
* **carry-over:** auto-prefill + available-to-claim bucket ([#39](https://github.com/sky-ai-eng/triage-factory/issues/39)) ([9954666](https://github.com/sky-ai-eng/triage-factory/commit/99546667a5bcf439cbb6e3d7263ce54f7934da44))
* **ci:** release pipeline + required CI checks + pure-Go SQLite ([#47](https://github.com/sky-ai-eng/triage-factory/issues/47)) ([f1bd3d9](https://github.com/sky-ai-eng/triage-factory/commit/f1bd3d969c6703fc7f846ecebdc742f78e1cc355))
* **db:** add prompt_triggers table for automated delegation ([#15](https://github.com/sky-ai-eng/triage-factory/issues/15)) ([2252ab1](https://github.com/sky-ai-eng/triage-factory/commit/2252ab1ca76d64e44264bcbe34401db592d18d26))
* **db:** rewrite schema for entity-first per-action event model (SKY-175) ([#20](https://github.com/sky-ai-eng/triage-factory/issues/20)) ([58405e7](https://github.com/sky-ai-eng/triage-factory/commit/58405e7ac8687013d3d90168956421f841b76cb0))
* **delegate:** add prompt selection support to backend ([3f41287](https://github.com/sky-ai-eng/triage-factory/commit/3f4128713f1d612f0f8a9465da1e1215bc1f5cff))
* **delegate:** add prompt stats view ([2170207](https://github.com/sky-ai-eng/triage-factory/commit/21702074b4d46a342a85f04cc9970d45e0ddc855))
* **delegate:** added prompts page ([49c17aa](https://github.com/sky-ai-eng/triage-factory/commit/49c17aa21675a5bddb73d59803331328805a94f9))
* **delegate:** added skills-as-prompts importer ([2641d9d](https://github.com/sky-ai-eng/triage-factory/commit/2641d9dc4e0848435dcbb055d67db8eddc4d5844))
* **delegate:** adjust PR review bot to include pricing information ([44cccc7](https://github.com/sky-ai-eng/triage-factory/commit/44cccc79980fbfc25f20db9a428c0ba7752334f8))
* **delegate:** event-driven auto-delegation with safety gates ([#17](https://github.com/sky-ai-eng/triage-factory/issues/17)) ([0b88d29](https://github.com/sky-ai-eng/triage-factory/commit/0b88d290dffa9dbb00e3c6e1aa32333a5b9c0fac))
* **delegate:** generalize envelope and delegation ([#5](https://github.com/sky-ai-eng/triage-factory/issues/5)) ([f09bfd6](https://github.com/sky-ai-eng/triage-factory/commit/f09bfd64d5d575c16a1889224f27397900de7360))
* **delegate:** graph wiring for default prompts ([ff8aa81](https://github.com/sky-ai-eng/triage-factory/commit/ff8aa8161a40b6a893569bc61f94cff810842905))
* **delegate:** persist task-specific memory ([#13](https://github.com/sky-ai-eng/triage-factory/issues/13)) ([9113885](https://github.com/sky-ai-eng/triage-factory/commit/9113885d594f0f25fa8da6433e17e2d78cce81c3))
* **delegate:** real prompt placeholders + workflow_run_id + list-runs (SKY-194) ([#42](https://github.com/sky-ai-eng/triage-factory/issues/42)) ([4fbdf34](https://github.com/sky-ai-eng/triage-factory/commit/4fbdf3461a4c91421c4969531956edd73614ba59))
* **delegate:** remove 'default' concept and just use 'auto' ([#18](https://github.com/sky-ai-eng/triage-factory/issues/18)) ([e34f504](https://github.com/sky-ai-eng/triage-factory/commit/e34f5044c1fc9e1394369d0a828572791eaab972))
* **delegate:** task_unsolvable status, circuit breaker, and global kill switch ([#16](https://github.com/sky-ai-eng/triage-factory/issues/16)) ([c697b41](https://github.com/sky-ai-eng/triage-factory/commit/c697b41ed0a7a55a84e351445a51d2ee8b1c171b))
* **docs:** add CLAUDE.md ([c088145](https://github.com/sky-ai-eng/triage-factory/commit/c088145659844f131f766607643d426f00d74dc7))
* **docs:** update repo documentation ([8ba5c4a](https://github.com/sky-ai-eng/triage-factory/commit/8ba5c4ae788dbc5b2fdd2ee198f71ed255531d8b))
* entity-first data model rewrite (SKY-174) ([c045f71](https://github.com/sky-ai-eng/triage-factory/commit/c045f71c630af58a2e832db2f1e74bcb46ceec4e))
* **events:** add full event and predicate registry ([db18f50](https://github.com/sky-ai-eng/triage-factory/commit/db18f509dab93c31929ed34a01ad981f98a8b7ea))
* **events:** add status predicate to jira assigned/available ([6d9a86b](https://github.com/sky-ai-eng/triage-factory/commit/6d9a86b4bd87b18e282b892e0ea35214cfb20c6b))
* **factory:** delegations now queue linearly based on events ([#45](https://github.com/sky-ai-eng/triage-factory/issues/45)) ([c170fd5](https://github.com/sky-ai-eng/triage-factory/commit/c170fd565a84b553ad0a470743792281cb4ccb09))
* **factory:** initial pass with dummy data ([#43](https://github.com/sky-ai-eng/triage-factory/issues/43)) ([b9546dd](https://github.com/sky-ai-eng/triage-factory/commit/b9546dd400c71675026b99d3fadf4ca8ec146a3e))
* **frontend:** overhaul dashboard and jira signup flow ([#7](https://github.com/sky-ai-eng/triage-factory/issues/7)) ([e66b754](https://github.com/sky-ai-eng/triage-factory/commit/e66b754d5646e6006f1e1ceace1a2900c4d70a04))
* **frontend:** trigger config panel on Prompts page (SKY-186) ([#27](https://github.com/sky-ai-eng/triage-factory/issues/27)) ([6d535c7](https://github.com/sky-ai-eng/triage-factory/commit/6d535c7282928788bb6aabd9c24e1e73228ba946))
* **gh:** `exec gh actions download-logs` + unified repo resolution ([#12](https://github.com/sky-ai-eng/triage-factory/issues/12)) ([75bbc6b](https://github.com/sky-ai-eng/triage-factory/commit/75bbc6bfbd5e47fcff7f87b0358c66259ef5cb3e))
* **github:** add PR tracking page ([120bf74](https://github.com/sky-ai-eng/triage-factory/commit/120bf7455ab63a02084de7f6527cd06561736209))
* **github:** added PR stats ([28df348](https://github.com/sky-ai-eng/triage-factory/commit/28df34872f61a826f9e4a130578402a798e26f8d))
* **github:** draggable draft state ([f98747f](https://github.com/sky-ai-eng/triage-factory/commit/f98747fc242b6c4b71b782e1d066492913c8cdac))
* **github:** emit `github_pr_new_commits` event ([#10](https://github.com/sky-ai-eng/triage-factory/issues/10)) ([202cbfa](https://github.com/sky-ai-eng/triage-factory/commit/202cbfaa8626a1eacdb215566e4ee30adf8dc139))
* **github:** track granular CI state for all PRs ([#11](https://github.com/sky-ai-eng/triage-factory/issues/11)) ([0d3e3a6](https://github.com/sky-ai-eng/triage-factory/commit/0d3e3a678431a6c61e15324ee497590e7dc3d30e))
* initial commit ([e8f83c7](https://github.com/sky-ai-eng/triage-factory/commit/e8f83c73ba12d5cc2f4eb9e9ed5687e8ba6ec8e7))
* **integrations:** reload pollers when keychain values change ([0cd2835](https://github.com/sky-ai-eng/triage-factory/commit/0cd2835d679f6d06ddb3115d73f97ffb9b6a3aa1))
* **jira:** add jira CLI shim ([34fc575](https://github.com/sky-ai-eng/triage-factory/commit/34fc575065a95efaf997ffb5f326586ebd738051))
* **jira:** add jira CLI shim ([#2](https://github.com/sky-ai-eng/triage-factory/issues/2)) ([34fc575](https://github.com/sky-ai-eng/triage-factory/commit/34fc575065a95efaf997ffb5f326586ebd738051))
* **jira:** claim guards for multi-task entities (SKY-183) ([#28](https://github.com/sky-ai-eng/triage-factory/issues/28)) ([378e626](https://github.com/sky-ai-eng/triage-factory/commit/378e626d2c49a3ad5d69149ac3e9c9100571bc41))
* **jira:** skip task creation for parents with open subtasks (SKY-173) ([#38](https://github.com/sky-ai-eng/triage-factory/issues/38)) ([fdef74f](https://github.com/sky-ai-eng/triage-factory/commit/fdef74f9b393bef08c82e1e7bb56279c63088448))
* **jira:** status rules — read sets vs canonical write (SKY-192) ([#32](https://github.com/sky-ai-eng/triage-factory/issues/32)) ([be7c0f5](https://github.com/sky-ai-eng/triage-factory/commit/be7c0f56bf4c5015e76c667732ebb4d8a607ad42))
* persist PRs and split tracking into a discover + diff solution ([#6](https://github.com/sky-ai-eng/triage-factory/issues/6)) ([05a4252](https://github.com/sky-ai-eng/triage-factory/commit/05a42526d20d8f709b04043ef6e4c265964a74ae))
* **prs:** subscribe to WS events for real-time updates (SKY-151) ([#37](https://github.com/sky-ai-eng/triage-factory/issues/37)) ([e95a260](https://github.com/sky-ai-eng/triage-factory/commit/e95a2606774a3a03d3b2fdc9bc137726c6348add))
* **repo:** rename the whole thang ([#19](https://github.com/sky-ai-eng/triage-factory/issues/19)) ([1466645](https://github.com/sky-ai-eng/triage-factory/commit/14666450aa9368f594560ae9d7b96387360d39b1))
* repos as first-class entities + profiling + system redesign ([#4](https://github.com/sky-ai-eng/triage-factory/issues/4)) ([149d65c](https://github.com/sky-ai-eng/triage-factory/commit/149d65ca3b80012be01c437173d8976f8b17d53e))
* **repos:** link present doc chips to the file on GitHub ([06a6d69](https://github.com/sky-ai-eng/triage-factory/commit/06a6d691b7c3f3c059db2d20ec1425a9190f8b4c))
* **repos:** redesign page with liquid-glass horizontal bands ([e3fef19](https://github.com/sky-ai-eng/triage-factory/commit/e3fef19073d90eb070e4f813977dcf368a0c8e47))
* **repos:** redesign page with liquid-glass horizontal bands ([b218c9c](https://github.com/sky-ai-eng/triage-factory/commit/b218c9cca99a6ec1183343b0ad3749b56a3e02f9))
* **review:** add db helpers ([5223439](https://github.com/sky-ai-eng/triage-factory/commit/522343915ae6a710edf3f7306795e6ecfb7856aa))
* **review:** add refractor syntax highlighting ([3b87073](https://github.com/sky-ai-eng/triage-factory/commit/3b870730760639c0027f4786a2cad6bf354b8c1a))
* **review:** add required CRUD routes for approval display ([163832a](https://github.com/sky-ai-eng/triage-factory/commit/163832a55b5730bff7a7b03afccf9f3a1450c452))
* **review:** add review components and dependencies ([ce16b18](https://github.com/sky-ai-eng/triage-factory/commit/ce16b1867f056bf0af2c547ce123bd3411d396ef))
* **review:** API endpoints for review approval flow ([93375ac](https://github.com/sky-ai-eng/triage-factory/commit/93375acfa62908e7297407bc4713e20bc41273a0))
* **review:** gate submit-review to defer when TODOTINDER_REVIEW_PREVIEW=1 ([8e57093](https://github.com/sky-ai-eng/triage-factory/commit/8e570939782d1188074700b39d39b36e41e85968))
* **review:** PR reviews await user approval instead of auto-posting ([d5bebe1](https://github.com/sky-ai-eng/triage-factory/commit/d5bebe15223f6a5e1c055909ef2902be94f0705d))
* **review:** set TODOTINDER_REVIEW_PREVIEW=1 env var in spawner ([939d4c3](https://github.com/sky-ai-eng/triage-factory/commit/939d4c37ad54ab61fe17f7205dca21847aedf043))
* **review:** show review summary in markdown or raw text ([66dc809](https://github.com/sky-ai-eng/triage-factory/commit/66dc80951f86ac8f4a60ab330f29492982157bb6))
* **routing:** autonomy-suitability gate + post-scoring re-derive (SKY-181, SKY-182) ([#26](https://github.com/sky-ai-eng/triage-factory/issues/26)) ([bef4d74](https://github.com/sky-ai-eng/triage-factory/commit/bef4d74e5755e47e17e9acacc1a807d75cc1dd12))
* **routing:** entity-first event pipeline (SKY-177 + SKY-178 + SKY-179) ([#23](https://github.com/sky-ai-eng/triage-factory/issues/23)) ([1190c16](https://github.com/sky-ai-eng/triage-factory/commit/1190c16d594a65eadbc19a5d9437408948c09ff0))
* **seed:** default trigger for auto PR review on review-requested ([8473cb3](https://github.com/sky-ai-eng/triage-factory/commit/8473cb3ff9771b1a605a7a20d898e7b182174b8e))
* **seed:** self-review loop prompts + starter triggers (SKY-160) ([#35](https://github.com/sky-ai-eng/triage-factory/issues/35)) ([20d8078](https://github.com/sky-ai-eng/triage-factory/commit/20d80786c25ce5393e9b9efb6fb09671f4e9aaed))
* **settings:** expose auto-delegate toggle ([35a2827](https://github.com/sky-ai-eng/triage-factory/commit/35a28278677437226f5338d4595a05ee3a24d1c6))
* **setup:** Jira carry-over step (SKY-191) ([#31](https://github.com/sky-ai-eng/triage-factory/issues/31)) ([6c18c1b](https://github.com/sky-ai-eng/triage-factory/commit/6c18c1b44c58efb68a9a1da1e39a7171a6f72dec))
* **task_rules:** backend CRUD API + frontend Triage page ([#24](https://github.com/sky-ai-eng/triage-factory/issues/24)) ([d5a3075](https://github.com/sky-ai-eng/triage-factory/commit/d5a30750bfc7b0ceb28cb528bc690b15adc1be78))
* toast notification system + Tier 1/2 consumers (SKY-187) ([698a6af](https://github.com/sky-ai-eng/triage-factory/commit/698a6af24a9edf1e036352d8da540e72fe4919e9))
* toast notification system + Tier 1/2 consumers (SKY-187) ([5f452ee](https://github.com/sky-ai-eng/triage-factory/commit/5f452ee8ca9d17b681018143ee8d798bdc46c4ff))
* **tracker:** backfill pr:review_requested on initial GH discovery ([c59cec2](https://github.com/sky-ai-eng/triage-factory/commit/c59cec2d1613d4bb9cc18b24def7949fb931506c))
* **triage:** adjust jira poller behavior and update state ([0d3c9fb](https://github.com/sky-ai-eng/triage-factory/commit/0d3c9fba5046b09caaaac841d5f2075df763e89b))
* **triage:** filter events ([419f17a](https://github.com/sky-ai-eng/triage-factory/commit/419f17a4b3f55b724c26968cc8107143a58bfdb7))
* **triage:** restructured things around a core event primitive ([cf050c5](https://github.com/sky-ai-eng/triage-factory/commit/cf050c55c2032e12f462ca5bf83fe96cc2a6b149))
* **triggers:** add PUT /api/triggers/{id} for config edits ([391024e](https://github.com/sky-ai-eng/triage-factory/commit/391024e2f642e87d266021cb44e526b76dd6bbca))


### Bug Fixes

* **ai:** unstick tasks in failed scoring batches ([2e83eae](https://github.com/sky-ai-eng/triage-factory/commit/2e83eaec2435bee15bbaaea121e3575ad6cc011c))
* **board:** show agent cards in done column too ([4cc77e7](https://github.com/sky-ai-eng/triage-factory/commit/4cc77e76320d3e3a3f246ae8d51647824141ac90))
* bug fixes ([9dd2aa4](https://github.com/sky-ai-eng/triage-factory/commit/9dd2aa446801f04074d2d9b9d3ff05abd9cac331))
* **dashboard:** patch PR snapshot after draft toggle (SKY-150) ([#36](https://github.com/sky-ai-eng/triage-factory/issues/36)) ([eb45de8](https://github.com/sky-ai-eng/triage-factory/commit/eb45de8a91506a4d2f38e1fda77999e54922bf55))
* **dashboard:** scope PRs page to user-authored PRs only ([#30](https://github.com/sky-ai-eng/triage-factory/issues/30)) ([4610b9c](https://github.com/sky-ai-eng/triage-factory/commit/4610b9c38dd6ea6b2f56e411cf8bd4bdc77c17a2))
* **db:** standardize columns ([8a9bbbd](https://github.com/sky-ai-eng/triage-factory/commit/8a9bbbdfae9a54db035211614481975485c94ddb))
* **db:** stray paren in taskColumnsWithEntity broke all task queries ([a798796](https://github.com/sky-ai-eng/triage-factory/commit/a798796f520c6c60fd951fe8b6332eabe6839c18))
* **delegate:** clean up ghost ~/.claude/projects entries after delegated runs ([ba1df6b](https://github.com/sky-ai-eng/triage-factory/commit/ba1df6bf74e38da8d24bd9bbf08a3ede2d48af9e))
* **delegate:** curated Bash allowlist + scratch-dir guidance (SKY-194) ([#41](https://github.com/sky-ai-eng/triage-factory/issues/41)) ([ce91584](https://github.com/sky-ai-eng/triage-factory/commit/ce91584c18d05df37620caddc88e5840cb2b947b))
* **delegate:** events trigger at the right times now ([37e9e51](https://github.com/sky-ai-eng/triage-factory/commit/37e9e51f35a8d9b0d3f98296cedbd73a5070c8f3))
* **delegate:** improve PR reviewer tone ([2c5a9a4](https://github.com/sky-ai-eng/triage-factory/commit/2c5a9a469b9b6acf5203adcd4bfe44b6e2bcaceb))
* **delegate:** update the spawner's credentials in place to preserve cancel list ([d29d832](https://github.com/sky-ai-eng/triage-factory/commit/d29d832c3b30f4dd38bb58acee62580fc7820738))
* **docs:** update docs (again) with future target state ([fa55a31](https://github.com/sky-ai-eng/triage-factory/commit/fa55a319ed5fa54aab0a443b052887fe144f035f))
* **events:** align Event struct, scope jira rule, rename breaker_threshold ([#22](https://github.com/sky-ai-eng/triage-factory/issues/22)) ([7ab087d](https://github.com/sky-ai-eng/triage-factory/commit/7ab087d7dc99cfc343d3ae75e9ed5de2e874c657))
* **factory:** scan NULL-able task text columns via NullString in active-runs query ([#44](https://github.com/sky-ai-eng/triage-factory/issues/44)) ([bb41bce](https://github.com/sky-ai-eng/triage-factory/commit/bb41bce1b26d321ba89c156e38209c0edb64a3cf))
* **frontend:** strip legacy fields, align with entity model (SKY-185) ([#25](https://github.com/sky-ai-eng/triage-factory/issues/25)) ([b05b6c7](https://github.com/sky-ai-eng/triage-factory/commit/b05b6c7425cb99e0030ba8e23b9ab2546a275293))
* **frontend:** UX + QOL ([be41566](https://github.com/sky-ai-eng/triage-factory/commit/be41566974ff178889c06d1af6e13f87b5c009b8))
* GitHub poller hitting node + runtime limits on discovery ([#14](https://github.com/sky-ai-eng/triage-factory/issues/14)) ([4025731](https://github.com/sky-ai-eng/triage-factory/commit/4025731b6876b77aca5a7ad18fbd59e2175ce537))
* **github:** fetch and worktree optimizations ([e697ea2](https://github.com/sky-ai-eng/triage-factory/commit/e697ea23ee0f11bcc40b3afc797174c198179590))
* **github:** isLocalID was checking if the first character was a digit ([57e9286](https://github.com/sky-ai-eng/triage-factory/commit/57e9286ca8f3effc3d450a1feb4363034b681fbe))
* **jira:** send correct field to unassign ([3d7adde](https://github.com/sky-ai-eng/triage-factory/commit/3d7adde7e51d69561f0181e163466d5de40615bc))
* more bug fixes ([a0a0a3b](https://github.com/sky-ai-eng/triage-factory/commit/a0a0a3b8614839ece69c7846379bfa7d64f17def))
* PR dashboard, profiling TTL, and closed PR event type ([#8](https://github.com/sky-ai-eng/triage-factory/issues/8)) ([98f05eb](https://github.com/sky-ai-eng/triage-factory/commit/98f05eb0b48fe8c5ff8155e69ecbda93b7613df1))
* **prompts:** default by exact event name, not prefix ([79390b4](https://github.com/sky-ai-eng/triage-factory/commit/79390b41e63ec0c626d77072e199b26c1e7943a7))
* **repos:** cancel stale BranchPicker fetches with AbortController ([785b933](https://github.com/sky-ai-eng/triage-factory/commit/785b93319496a1614a39373f61dea43985934e95))
* **repos:** classification step improvement ([ab1f52b](https://github.com/sky-ai-eng/triage-factory/commit/ab1f52b8bb82f149821f4833db4fd8d69345cacb))
* **repos:** clean up BranchPicker debounce + fetch on unmount ([48c7f83](https://github.com/sky-ai-eng/triage-factory/commit/48c7f830a401433596d01c91f8a2983b64462616))
* **repos:** derive doc chip URLs from configured GitHub base URL ([37e64da](https://github.com/sky-ai-eng/triage-factory/commit/37e64daa230c92fca69d9fd8353cf746711d97fc))
* **repos:** highlight the effective branch, not raw base_branch ([c690881](https://github.com/sky-ai-eng/triage-factory/commit/c690881e6c6ad1ba2ff1750d9c8d9d3837d1c555))
* **repos:** the stored GitHub base URL is already the web root ([4788031](https://github.com/sky-ai-eng/triage-factory/commit/4788031a5cee62c5b0087399ee6e52f7e05c5c57))
* **review:** adapter to unwrap refractor.highlight(...).children ([95b06e4](https://github.com/sky-ai-eng/triage-factory/commit/95b06e4de7ffef154aaa429845e67d9e055ed919))
* **review:** darken diff colors ([b54b7b8](https://github.com/sky-ai-eng/triage-factory/commit/b54b7b8de2031a358ee4f0cf7e9638133797c06b))
* **review:** diff highlighting CSS classes ([29edbe5](https://github.com/sky-ai-eng/triage-factory/commit/29edbe568a4137d6895b9a2049b731bcaa372ddf))
* **review:** reformat existing diff view styling ([20cf4e2](https://github.com/sky-ai-eng/triage-factory/commit/20cf4e2f4588820b314d4aeae8230a0d9c420f3e))
* **review:** refractor imports ([cfa90f8](https://github.com/sky-ai-eng/triage-factory/commit/cfa90f81e9bbf9112387e45c4892c79609319ebd))
* **review:** use actual - not computed - cost for review body ([d0d71fd](https://github.com/sky-ai-eng/triage-factory/commit/d0d71fdf929a366e944d30130b4bd4eb819f6fce))
* **setup:** rewrite integrations step as multi-screen Jira flow (SKY-188) ([#29](https://github.com/sky-ai-eng/triage-factory/issues/29)) ([5098a8a](https://github.com/sky-ai-eng/triage-factory/commit/5098a8a5f4845d64c20deef80ec4f03f14be6961))
* **stock:** apply SKY-173 subtask gate to carry-over ([0982cb3](https://github.com/sky-ai-eng/triage-factory/commit/0982cb32240ce43c84c58659f4d12aab928efc3a))
* **TaskRulesPanel:** long rule names overflow the 340px panel ([150ca49](https://github.com/sky-ai-eng/triage-factory/commit/150ca49b57dc997c260083dd2f51b878108ebbea))
* **toast:** handle typed-nil hubs without panicking ([c0a8ed8](https://github.com/sky-ai-eng/triage-factory/commit/c0a8ed8aa7e9cc4bac455180001d2e03d8c7a9ed))
* **toast:** report exact skipped-task count from failed scoring batches ([a3d49d0](https://github.com/sky-ai-eng/triage-factory/commit/a3d49d0c2db8693998243cae570b2293a17cd87c))
* **toast:** throttle poll-failure toasts per source ([c4e41ec](https://github.com/sky-ai-eng/triage-factory/commit/c4e41ec191a41947054c61a37d1016626d5a4215))
* **tracker:** bump reviewRequests cap from 10 to 100 ([#40](https://github.com/sky-ai-eng/triage-factory/issues/40)) ([019ad90](https://github.com/sky-ai-eng/triage-factory/commit/019ad90552b74cb5d3e6cc016a006d259346494c))
* **tracker:** stamp PR review-request backfill with PR.CreatedAt ([19e1ea1](https://github.com/sky-ai-eng/triage-factory/commit/19e1ea156cb1b4eca44c7bdd4ab10f3eeb3bd73a))
