# Local go-mitmproxy v1.9.2 maintenance

The runtime source and upstream license were copied from the existing module
cache. CHENGJIAN enables DisableSSEParsing: raw SSE bytes still pass unchanged
through StreamResponseModifier and are recorded by the bounded streaming sink.
This avoids the upstream parser retaining every event or an unbounded event
without a delimiter. Other users retain the upstream default parsing behavior.
The preserved upstream project and shared module cache are unchanged.
