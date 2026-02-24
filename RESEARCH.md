
Active research areas:
- [Prompt routing and tiered execution](#tiers)
- Speculative decoding
- Knowledge preamble
  - [CLaRa for better knowledge retrieval](#clara)
  - BGE-large reranking
- Auto-build and LSP feedback
- ZeroLLM (cmd execution intent, T3 compiled skills)

## Tiers

oc has 4 prompt-execution tiers, moving up or down based on interaction to reduce LLM round-trips and tool calls.

> Example 1: "Find the libuv compat layer in this project"

A "knowledge preamble" is injected into the system prompt to reduce/eliminate discovery tool calls. RAG embeddings and local models are used to construct a small preamble useful for the particular prompt. 

The model is encouraged to use the "Code tool" instead of several consecutive tool calls. For this case, the model is able to gather all the required information using a single code tool call. This script is directly
promoted to Tier 3. Tier 3 scripts may have built-in "deopts" sites that can fallback to lower tiers for non-deterministic operations, thinking and reasoning. Future prompts that get classified as similar
to previously compiled script get executed directly.

> Example 2: "Changes look good. Can you verify the tests pass?"

Before handing off this prompt to the LLM oc classifies it as EXEC_INTENT, it then does a embedding search to find similar commands from past sessions, If found it goes to a local LLM for confidence, high confidence commands are executed directly, low confidence commands are injected into the preamble so the LLM can notice 
and maybe reuse it. In the best case this avoids several tool calls to discover the test suite and generates a Tier 3 compiled skill; worst case we have 2 compiled skills: one for discovery and another for running the test suite.

> Example 3: "Implement google oauth next. Make sure people get auto-assigned orgs and can invite others to join"

This is a fairly large task. Other tiers will kick in for subtasks when appropriate but oc also maintains a global RAG knowledge base. Discovery and refactoring may promote up to Tier 3 as compiled skills via the code tool.
This on average consumes 30% less tokens than other tools.

RLHF training bias: Most models are trained on atomic tool-use patterns. Code tools requires planning the operations upfront. This sometimes leads to models avoiding the code tool, oc may choose to disable atomic tools if it sees the model is overusing them.

## CLaRa

TL;DR: this has been disappointing. 

I wanted to use Apple's [CLaRa](./papers/clara.pdf) (Continuous Latent Reasoning) for context stuffing in oc. It would ingest all known patterns as documents and figure out the closest match. The architecture looks great on paper as it replaces the normal RAG cascade and has good compression. A Python sidecar that loads the `apple/CLaRa-7B-E2E` pretrained model on MPS and compressed all traces as documents. CLaRa's whole point is that retrieval and generation are joint, the differentiable top-k is supposed to let gradients flow from generation loss back through retrieval, so the retriever learns what the generator needs.

The results were mixed when I actually tested it: 58% accuracy for 100 queries. These queries as formulated as factoid questions since it's a QA model. I need to investigate further but I feel like retrival is just bad and CLaRa relies on a good retriever like the BGE-large and the base model's parametric knowledge. CLaRa's own benchmarks measure end-to-end answer accuracy (EM/F1), but the generator is using Mistral's parametric knowledge, not the retrieved documents which leads me to believe the it's own retrieval is just broken? Also makes me a bit skeptical about using latent space for RAG.

