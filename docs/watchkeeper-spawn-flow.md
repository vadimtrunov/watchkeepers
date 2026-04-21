# Watchkeeper Spawn Flow

This document describes the end-to-end flow for spawning a new Watchkeeper into a Slack workspace, from the human's request to the moment the agent goes live.

## Trigger

A human writes to the Watchmaster, e.g. *"I need a code review Watchkeeper for the backend team."* The Watchmaster parses the request and kicks off the spawn process.

## Steps

### Step 1 — Manifest

The Watchmaster generates a Manifest for the new Watchkeeper: system prompt, toolset, role definition, authority matrix, and knowledge sources. It either:

- pulls a template from the Role Catalog, or
- composes a custom Manifest based on the request.

The Manifest is submitted to the human lead for approval. Nothing else happens until the human approves.

### Step 2 — Slack App Creation

Once the Manifest is approved, the **platform compiled core** (not the Watchmaster) creates a new Slack App via the Slack Manifest API — the API that allows programmatic app creation from a JSON configuration. The request specifies:

- app name and description,
- required OAuth scopes (`channels:read`, `chat:write`, `users:read`, etc.),
- event subscriptions.

Slack returns the `app_id` and credentials.

### Step 3 — Installation

The platform installs the app into the target workspace via the OAuth flow. For internal apps running on Enterprise Grid, installation can be fully automated through admin approval policies: if the org admin has pre-approved apps from the Watchkeeper Platform, no manual click is required.

### Step 4 — Bot Profile Setup

The platform configures the bot user — display name, avatar, description — via `bots.info` and `users.profile.set`. The Watchkeeper now appears in the workspace as a new member.

### Step 5 — Runtime Launch

The platform starts the runtime for the new Watchkeeper with its Manifest, attaches the bot token, and subscribes it to the relevant events and channels. The Watchkeeper comes alive and posts its introduction message, e.g.:

> *"Hi, I'm the Code Reviewer. I'll be reviewing PRs for the backend team."*

## Separation of Concerns

Steps 2–4 are executed by the **compiled core** of the platform, never by the Watchmaster. The Watchmaster's responsibility ends at the decision (*"what to spawn"*) and the Manifest it produces. The physical act of creating a Slack App and issuing credentials is a privileged operation that lives in the immutable core. The Watchmaster invokes it as a tool but cannot modify the creation process itself.

This is a direct application of the dual-language architecture: agent-facing logic (Watchmaster, tools, prompts) sits in the interpreted layer, while app-provisioning and credential issuance live in the compiled core and are unreachable from agent code.

## Slack Manifest API Notes

The Slack Manifest API requires an app-level token with the `app_configuration` scope. The model is:

- **One parent app** — the Watchkeeper Platform itself — holds the `app_configuration` token.
- The parent app creates **child apps**, one per Watchkeeper, on demand.

This is officially supported, but the platform must respect Slack's app-creation rate limits. For realistic operating volumes (a handful of Watchkeepers spawned per day), this is not a practical concern; higher-volume deployments should account for it explicitly.
