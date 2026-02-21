oc has 4 prompt-execution tiers, moving up or down based on interaction to reduce LLM round-trips and tool calls.

Example 1: "Find the libuv compat layer in this project"

<img width="1511" height="768" alt="image" src="https://github.com/user-attachments/assets/9a144cbf-a6be-47c9-8cc0-1d7998e08182" />

A "knowledge preamble" is injected into the system prompt to reduce/eliminate discovery tool calls. RAG embeddings and local models are used to construct a small preamble useful for the particular prompt. 

The model is encouraged to use the "Code tool" instead of several consecutive tool calls. For this case, the model is able to gather all the required information using a single code tool call. This script is directly
promoted to Tier 3. Tier 3 scripts may have built-in "deopts" sites that can fallback to lower tiers for non-deterministic operations, thinking and reasoning. Future prompts that get classified as similar
to previously compiled script get executed directly.

Example 2: "Changes look good. Can you verify the tests pass?"

A few sessions ago you might asked the LLM to do this same thing but it doesn't have that context anymore. Before handing off this prompt to the LLM oc classifies it as EXEC_INTENT, it then does a embedding search to find
similar commands from past sessions, If found it goes to a local LLM for confidence, high confidence commands are executed directly, low confidence commands are injected into the preamble so the LLM can notice 
and maybe reuse it. In the best case this avoids several tool calls to discover the test suite and generates a Tier 3 compiled skill; worst case we have 2 compiled skills: one for discovery and another for running the test suite.

Example 3: "Implement google oauth next. Make sure people get auto-assigned orgs and can invite others to join"

This is a fairly large task. Other tiers will kick in for subtasks when appropriate but oc also maintains a global RAG knowledge base, in other tools this is similar to a AGENTS.md file. Discovery and refactoring may promote up to Tier 3 as compiled skills via the code tool.
This on average consumes 30% less tokens than other tools.

RLHF training bias: Most models are trained on atomic tool-use patterns. Code tools requires planning the operations upfront. This sometimes leads to models avoiding the code tool, oc may choose to disable atomic tools if it sees the model is overusing them.
