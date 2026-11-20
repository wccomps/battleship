# Spec Delta

## Purpose

Gives staff what they need to run self-service sessions without attending them: seeing every booking, approving new customers, stopping and refunding sessions, blocking customers and dates, trying environments for free, and being told when something needs a person.

## ADDED Requirements

### Requirement: Staff see every booking and session
The system SHALL show staff every booking with its host, window, environment, pods, payment and current state, and the record of what was done to it and by whom.

#### Scenario: Staff look at a running session
- **WHEN** a staff member opens the bookings page during a session
- **THEN** it shows the running session, its pods and each step taken since Start

### Requirement: A new customer's first booking needs staff approval before payment
The system SHALL hold the window of a first booking from an email address that staff have not approved, SHALL ask staff to approve or refuse it, and SHALL take the host to payment only after approval. An approved address SHALL go straight to payment on later bookings. An unanswered request SHALL lapse and free its window.

#### Scenario: First booking from a new address
- **WHEN** a host books from an address staff have not approved
- **THEN** the window is held, staff are asked to decide, and the host is told payment follows approval

#### Scenario: Staff approve
- **WHEN** staff approve the request
- **THEN** the host is emailed a link to pay and later bookings from that address need no approval

#### Scenario: Staff refuse or do not answer
- **WHEN** staff refuse the request, or the approval period passes with no decision
- **THEN** the window becomes free and the host is told no payment was taken

### Requirement: Staff can end any session at once
The system SHALL let staff end a running or starting session immediately, following the same ordered end as any session, and SHALL record who ended it and why.

#### Scenario: Staff end a session for abuse
- **WHEN** a staff member ends a running session
- **THEN** access is cut and the pods are torn down as at a normal end, and the reason is recorded

### Requirement: Staff can refund a booking
The system SHALL let staff refund any paid booking in full, whatever its state, and SHALL record who refunded it and why.

#### Scenario: Session failed part-way on the org's side
- **WHEN** a running session is lost to a fault on the org's side and a staff member refunds it
- **THEN** the full amount is returned and the host is emailed

### Requirement: Staff decide refunds for sessions that fail while running
The system SHALL alert staff when a running session loses its pods or its scoring for a reason on the system's side, and SHALL leave the refund to staff.

#### Scenario: Scoring stops answering mid-session
- **WHEN** a running session's scoring stops answering and does not recover
- **THEN** staff are alerted with the booking, and no refund is made until a staff member decides

### Requirement: Staff can block a customer
The system SHALL let staff block an email address from booking, and SHALL refuse new bookings from a blocked address before payment. Blocking SHALL NOT by itself cancel bookings already paid.

#### Scenario: Blocked address tries to book
- **WHEN** a blocked email address requests a window
- **THEN** the request is refused before payment

### Requirement: Staff can close dates
The system SHALL let staff close any span of time to sessions. Closing a span that holds a confirmed booking SHALL cancel and fully refund that booking and email the host, as an official event does.

#### Scenario: Maintenance weekend
- **WHEN** staff close a weekend for maintenance
- **THEN** no window in it can be booked, and a booking already confirmed in it is cancelled and refunded

### Requirement: Staff can run a session without payment
The system SHALL let staff create a booking with no payment, subject to every other booking rule, and SHALL let a staff booking use an environment that is complete but not yet published.

#### Scenario: Trying a new environment
- **WHEN** a staff member creates a staff booking for a complete, unpublished environment in a free window
- **THEN** the booking is confirmed without payment and can be started like any other

#### Scenario: Staff booking over a customer's window
- **WHEN** a staff member tries to create a staff booking that overlaps a customer's booking
- **THEN** the system refuses, as it would for a customer

### Requirement: Staff are told when a person is needed
The system SHALL post to the staff operations channel when a session cannot be cleaned up, when a start fails, when a refund could not be decided or made, when a new customer awaits approval, and when a published environment is withdrawn. Each notice SHALL name the booking and what is needed.

#### Scenario: Teardown keeps failing
- **WHEN** a session's teardown has failed and is being retried
- **THEN** the staff operations channel shows the booking, the failing step and that bookings are closed
