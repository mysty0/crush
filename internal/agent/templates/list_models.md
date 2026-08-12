List the models available for the `model` parameter of the `agent`, `Workflow`, and `agentic_fetch` tools.

Those tools name only a few common models directly. Call this tool when you need a model they do not mention — a different provider, a cheaper or faster tier, or a specialized model.

Filter the results rather than listing everything: pass `provider` to restrict to one provider, and/or `filter` to match model IDs containing a substring (e.g. "haiku", "gpt-5"). With no arguments this returns every available model, which can be a long list.

Pass a returned model ID verbatim as the `model` parameter.
