# Tasks

## 1. Selling the add-on

- [ ] 1.1 Let an environment be published with a rubric per inject and a red team action list, checked together; verify an environment missing either is published without the add-on
- [ ] 1.2 Offer the add-on at checkout only for environments that carry it, add $10 to the price, and refuse adding or removing it after payment or switching to an environment without it; verify with one test per scenario of "The add-on is chosen at booking, for environments that carry it" and the pricing scenarios
- [ ] 1.3 Show the add-on notice at checkout and on pod pages before Start, and name the AI provider in the terms; verify the notice appears before any sign-in details

## 2. AI grading

- [ ] 2.1 Read new submissions from the session's scoring in the worker and take their text out with size, page and time limits; verify an oversized or malformed file is marked not graded and nothing else fails
- [ ] 2.2 Grade with no tools, a fixed reply shape, scores clamped in code, the per-inject cap and the per-booking budget; verify with property tests that no submission text moves a score outside its bounds, and that submissions past the cap or budget are kept and marked not graded
- [ ] 2.3 Show a pod its scores and feedback as escaped text on its link; verify with an escaping test that markup in feedback is not rendered, and with the views harness that no other pod can see them

## 3. Red team

- [ ] 3.1 Build the runner container: named actions only, an address and action name in, a report out, no AI key and no route to the internet; verify it refuses an unknown action and cannot open a connection outside the pod networks
- [ ] 3.2 Have the scoring VM's agent start the runner for add-on sessions and remove it at wipe; verify a session without the add-on has no runner, and that none survives End
- [ ] 3.3 Drive the runner from the worker: compute the target from the booking's pods before each action, check the session is running, skip a VM that was reset until it is up, cancel at End; verify with tests that a target's reply never becomes a command or address
- [ ] 3.4 Store findings against booking and pod and show a pod its own with its results; verify with the views harness that a pod sees no other pod's findings and the scoreboard shows none
- [ ] 3.5 Allow the runner into the booking's pod networks only in add-on sessions; verify from the runner that a pod of the booking is reachable and that an official pod, the management network and an internet address are not

## 4. Whole add-on

- [ ] 4.1 Run a staff session with the add-on against the real systems: submit an inject, receive graded feedback, see a red team finding, end; verify afterwards that no runner, key or submission remains on the scoring VM
- [ ] 4.2 Add the add-on's entry points and the AI provider to the practice app's threat model; verify its citations test passes
