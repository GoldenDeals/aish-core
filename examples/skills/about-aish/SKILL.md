---
name: about-aish
description: Explains what aish is and how its modules, events and policies fit together. Use when the user asks about aish itself.
---

# About aish

aish is an agent harness written in Go. Everything except the event bus is a module: the agent loop, the frontend, the LLM provider, tools and policies.

- Modules never call each other. They subscribe to events (`tool.call`, `llm.request`, ...) with a mode (Mutate, Handle or Observe) and a numeric priority.
- A policy is a Mutate hook on `tool.call` at priority 1000. It lets the call through, asks the user, or denies it.
- The configuration file is called `Agentfile` and uses Caddyfile syntax. `aish list-modules` shows the linked modules, `aish list-hooks` shows every subscription of a configured session.
- Design details are in `docs/ARCHITECTURE.md`.
