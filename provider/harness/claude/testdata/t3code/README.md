# Recorded quota fixtures

Synthesized fixtures shaped like the SDK `get_usage` response and the
streamed `rate_limit_event`, using the shapes the t3code claude
usage-limits module documents (0–100 percentages with ISO resets on the
read; a 0–1 utilization fraction with an epoch-seconds reset on the
event). No upstream fixture files existed to copy; the mapping rules
they pin come from the MIT-licensed t3code provider usage-limits
modules (see `../../LICENSE-t3code`). Values are synthetic.
