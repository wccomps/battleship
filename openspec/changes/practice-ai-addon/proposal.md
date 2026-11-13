# Proposal

## Why

A practice session has no white team to grade injects and no red team to attack the boxes, so teams get service scores and little else. For $10 more, an AI can do both: grade each inject submission against a rubric, and run a scripted red team against the session's pods. It was asked for as a bonus on top of self-service sessions, and it is kept apart from them because it carries their riskiest parts.

## What Changes

- **A $10 add-on**, chosen at booking, offered only for environments that carry its files.
- **AI grading.** Each inject submission is scored against that inject's rubric, item by item, with written feedback shown on the pod's link.
- **A scripted red team.** It runs a fixed list of actions, written by staff for the environment, against the booking's pods only, and reports to each pod what it achieved against that pod.
- **Notice.** The host at checkout and every sub-team before the session are told that submissions go to an AI provider and that their pod will be attacked.
- **Environments gain two optional parts**: a rubric per inject and a red team action list.

This change depends on `self-service-team-play` and is built after it. Sessions work without it.

## Capabilities

### New Capabilities
- `ai-event-automation`: The paid add-on: who gets it, AI grading of inject submissions, a bounded scripted red team, and what each pod is shown.

### Modified Capabilities
- `play-session-booking`: the price gains the optional $10 add-on.
- `environment-lifecycle`: in add-on sessions the red team is allowed to open connections into the booking's pods.

## Impact

- **The practice app**: add-on choice at checkout, environment upload of rubrics and action lists, grading in the worker, pages for feedback and findings, an AI provider key.
- **The practice scoring VM**: a red team runner container, started for add-on sessions and removed at End.
- **A new outside service**: an AI provider. Student submissions leave the org.
- **Network**: the runner may reach the booking's pods and nothing else, the internet included.
