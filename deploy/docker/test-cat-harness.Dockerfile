# syntax=docker/dockerfile:1.6
#
# F2 verification fixture (NOT a product image). A generic-cli harness image:
# /agent is the run entrypoint (copied from the agent image, a static binary),
# and busybox provides `cat`. An AgentRun with `inputs` materializes a file into
# the workspace; the harness runs `cat <file>` and returns its content, proving
# F2's positive path (the input file is materialized AND read by a harness) with
# no S3/AgentFS — just harness.cli.workingDir.
ARG AGENT_IMAGE=ghcr.io/smol-platform/smol-agents/agent:0.1.13
FROM ${AGENT_IMAGE} AS agentbin
FROM busybox:1.36
COPY --from=agentbin /agent /agent
ENTRYPOINT ["/agent"]
