# Spec Delta

## Purpose

Delivers the optional $10 add-on for a session: inject submissions graded by an AI against the environment's rubric, and a scripted red team that attacks the session's pods and reports what it achieved.

## ADDED Requirements

### Requirement: The add-on runs only for bookings that bought it
The system SHALL run AI grading and the AI red team only for sessions whose booking paid for the add-on.

#### Scenario: Booking without the add-on
- **WHEN** a pod in a session without the add-on submits an inject
- **THEN** the submission is kept with the pod's results and nothing is graded or attacked

### Requirement: The add-on is chosen at booking, for environments that carry it
The system SHALL offer the add-on only for an environment published with a rubric for each inject and a red team action list. The host SHALL choose it before paying, and it SHALL NOT be added to or removed from a paid booking.

#### Scenario: Environment without add-on files
- **WHEN** a host books an environment published without rubrics or a red team action list
- **THEN** the add-on is not offered for that booking

#### Scenario: Host wants the add-on after paying
- **WHEN** a host with a paid booking asks to add the add-on
- **THEN** the booking page offers cancelling for a refund and booking again, and no way to add it

#### Scenario: Host switches to an environment without add-on files
- **WHEN** a host whose booking includes the add-on chooses an environment that does not carry it
- **THEN** the system refuses the change

### Requirement: Everyone in an add-on session is told what it does
The system SHALL tell the host at checkout, and each sub-team on its pod link before the session starts, that submissions will be sent to an AI provider for grading and that the pod will be attacked by an automated red team.

#### Scenario: Sub-team opens its link
- **WHEN** a sub-team opens its link for a booking with the add-on
- **THEN** the page states both facts before any sign-in details are shown

### Requirement: Inject submissions are graded against the environment's rubric
The system SHALL grade each inject submission from an add-on session against that inject's rubric, giving each rubric item a score between zero and its maximum, and SHALL show the pod its score and written feedback on its link.

#### Scenario: Submission graded
- **WHEN** a pod submits an inject response before the inject closes
- **THEN** the pod's link later shows a score per rubric item, none above its maximum, and feedback

### Requirement: A submission cannot change how it is graded
The content of a submission SHALL NOT be able to alter the rubric, raise a score above an item's maximum, or cause any action other than producing a score and feedback. Feedback SHALL be shown as plain text.

#### Scenario: Submission tells the grader to award full marks
- **WHEN** a submission contains text instructing the grader to ignore the rubric
- **THEN** the score still lies within each item's bounds and nothing else in the system changes

#### Scenario: Feedback contains markup
- **WHEN** generated feedback contains HTML or script
- **THEN** the pod's link shows it as text and runs none of it

### Requirement: Grading is bounded per booking
The system SHALL grade at most a fixed number of submissions per inject per pod and SHALL stop grading for a booking once its grading budget is spent, telling the pod which submissions were not graded.

#### Scenario: Pod resubmits repeatedly
- **WHEN** a pod submits the same inject more times than the limit
- **THEN** later submissions are kept and marked as not graded

### Requirement: The red team attacks only the booking's pods with fixed actions
The AI red team SHALL act only against addresses inside the pods of its own booking, checked again before each action, and SHALL choose its actions only from the environment's fixed list. Nothing a target sends back SHALL become a command or a new target.

#### Scenario: Action against a pod of the booking
- **WHEN** the red team runs an action from the environment's list against a pod of its booking
- **THEN** the action runs and its outcome is recorded for that pod

#### Scenario: Target reply names another address
- **WHEN** a target's reply contains another address or a command
- **THEN** the red team neither connects to that address nor runs that command

### Requirement: The red team stops when the session or the VM stops
The AI red team SHALL stop acting against a pod at once when the session ends, and SHALL not resume against a VM that was reset until the VM is running again.

#### Scenario: Session ends mid-action
- **WHEN** the session ends while a red team action is running
- **THEN** the action is cancelled and no further traffic is sent

### Requirement: Each pod gets its red team findings
The system SHALL record what the red team achieved against each pod, with the time, the VM and the action, and SHALL show a pod its own findings with its results after the session.

#### Scenario: Results after an add-on session
- **WHEN** an add-on session ends
- **THEN** each pod's link lists the red team's findings against that pod and none against other pods
