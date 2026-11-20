# Spec Delta

## Purpose

Governs how a host books and pays for a self-service play session: the window lengths and prices, when a window may be booked, how payment confirms it, when money is returned, and how the host manages the booking without an account.

## ADDED Requirements

### Requirement: Play window duration and pricing tiers
The system SHALL offer a 4-hour half-play window at $40 USD and an 8-hour full-play window at $80 USD. The price SHALL be computed by the system from the chosen window, and SHALL NOT depend on the number of sub-teams.

#### Scenario: Half play
- **WHEN** a host chooses a 4-hour window
- **THEN** the amount to pay is $40 USD

#### Scenario: Full play
- **WHEN** a host chooses an 8-hour window
- **THEN** the amount to pay is $80 USD

#### Scenario: A price sent by the browser is ignored
- **WHEN** a booking request carries an amount that differs from the system's price for its window
- **THEN** the system charges its own price

### Requirement: One session at a time
The system SHALL accept at most one booking for any time span. A booking's span runs from its start to its end plus the teardown gap that follows every session.

#### Scenario: Overlapping window
- **WHEN** a host requests a window that overlaps another booking's span
- **THEN** the system refuses the request before payment and shows the nearest free windows

#### Scenario: Back-to-back windows
- **WHEN** a host requests a window that starts after another booking's end but inside its teardown gap
- **THEN** the system refuses the request before payment

### Requirement: Windows start on the hour, in Pacific time, up to 60 days ahead
The system SHALL offer windows that start on the hour, SHALL show and accept all times in Pacific time and say so, and SHALL refuse a window that has already begun or that starts more than 60 days ahead.

#### Scenario: Window too far ahead
- **WHEN** a host requests a window starting 75 days from now
- **THEN** the system refuses the request before payment

#### Scenario: Start time off the hour
- **WHEN** a host requests a window starting at 10:20
- **THEN** the system refuses the request and offers 10:00 and 11:00

### Requirement: Official event blackout
The system SHALL refuse any window that falls within 7 days before an official event's date, or while an official event is open. After an official event is closed out, bookings SHALL stay closed until staff confirm that the event's pods are torn down.

#### Scenario: Window in the week before an event
- **WHEN** a host requests a window that ends 3 days before an official event's date
- **THEN** the system refuses the request before payment

#### Scenario: Event closed out, teardown not confirmed
- **WHEN** an official event has been closed out and staff have not yet confirmed its pods are torn down
- **THEN** no window can be booked and no confirmed session can be started

#### Scenario: Staff confirm teardown
- **WHEN** staff confirm teardown and no official event is open or within 7 days
- **THEN** windows can be booked again

#### Scenario: Official event dates cannot be read
- **WHEN** the system has been unable to read the official event dates for more than a day
- **THEN** no window can be booked, no session can be started, and staff are alerted

### Requirement: Bookings displaced by an official event are refunded
The system SHALL cancel and fully refund a confirmed booking whose window comes to fall inside a blackout because an official event was scheduled or moved after the booking was paid, and SHALL email the host.

#### Scenario: Event scheduled over a paid booking
- **WHEN** staff create an official event dated 4 days after a confirmed booking's window
- **THEN** the booking is cancelled, the full amount is refunded, and the host is emailed the reason

### Requirement: Payment confirms the booking
The system SHALL hold a requested window as pending until the payment provider reports the payment, and SHALL confirm it only on a report whose authenticity it has verified and whose amount and currency equal the booking's price. A browser returning from checkout SHALL NOT confirm a booking.

#### Scenario: Verified payment report
- **WHEN** the payment provider delivers a verified report of full payment for a pending booking
- **THEN** the booking becomes confirmed and the host is emailed the booking link

#### Scenario: Forged or altered report
- **WHEN** a payment report fails verification, or names an amount or currency other than the booking's price
- **THEN** the booking stays pending and the report is recorded as refused

#### Scenario: Same report delivered twice
- **WHEN** a verified payment report is delivered a second time
- **THEN** nothing changes and no second email is sent

### Requirement: Unpaid holds are limited
The system SHALL release a pending booking's window when it is not paid within the hold period, SHALL allow one pending booking per email address at a time, and SHALL limit how many pending bookings one network may hold.

#### Scenario: Hold expires
- **WHEN** a pending booking is not paid within the hold period
- **THEN** its window becomes free again

#### Scenario: Second pending booking from the same address
- **WHEN** an email address with a pending booking requests another window
- **THEN** the system refuses the second request

#### Scenario: Many addresses from one network
- **WHEN** requests from one network, each under a different email address, reach that network's limit of pending bookings
- **THEN** the system refuses further requests from that network until one is paid or expires

### Requirement: Host manages the booking through an emailed link
The system SHALL give the host control of a booking through a secret link sent to the email address given at booking, with no account or password. Possession of the link SHALL be the only way to manage the booking, and every change made through it SHALL be recorded.

#### Scenario: Host opens the link
- **WHEN** the host opens the emailed link
- **THEN** the host sees the booking and can add or remove sub-teams, choose the environment, and start or end the session

#### Scenario: Unknown or expired link
- **WHEN** a request presents a link that matches no booking, or one that has expired or been replaced
- **THEN** the system answers as it does for a page that does not exist

#### Scenario: Host asks for a new link
- **WHEN** a new link is requested for a booking
- **THEN** it is sent only to the booking's email address and the previous link stops working

### Requirement: The host can cancel for a full refund until someone signs in
The system SHALL let the host cancel a booking for a full refund at any time before any of the booking's session accounts has signed in. Cancelling a session that has started SHALL also end it. Once any session account of the booking has signed in, no refund SHALL be given.

#### Scenario: Cancel before Start
- **WHEN** the host cancels a confirmed booking that has not been started
- **THEN** the booking is cancelled, its window becomes free, and the full amount is refunded

#### Scenario: Cancel after Start, before any sign-in
- **WHEN** the host cancels a started session and none of its session accounts has signed in
- **THEN** the session is ended, its pods are torn down, and the full amount is refunded

#### Scenario: Cancel after a sign-in
- **WHEN** the host cancels a session after any of its session accounts has signed in
- **THEN** the session is ended and no refund is given

#### Scenario: Sign-ins cannot be checked
- **WHEN** the host cancels a started session and the system cannot tell whether anyone has signed in
- **THEN** the session is ended, no refund is made automatically, and staff are alerted to decide it

### Requirement: A session that fails to start is refunded
The system SHALL fully refund a booking whose session could not be started for a reason on the system's side, and SHALL email the host.

#### Scenario: Pods cannot be deployed
- **WHEN** the host presses Start and the pods cannot be deployed
- **THEN** whatever was created is removed, the booking is refunded in full, and the host is emailed

### Requirement: A booking that is never started is refunded
The system SHALL fully refund a confirmed booking whose window ends without Start having been pressed, and SHALL email the host.

#### Scenario: Window passes unused
- **WHEN** a confirmed booking's window ends and the host never pressed Start
- **THEN** the booking is closed, the full amount is refunded, and the host is emailed

### Requirement: A booking's window cannot be moved
The system SHALL NOT let a host change a paid booking's date or window length. The host SHALL be able to choose a different environment until Start. To play at another time the host cancels and books again.

#### Scenario: Host wants another date
- **WHEN** a host with a confirmed booking wants a different date
- **THEN** the booking page offers cancelling for a refund and booking again, and no way to move the window

#### Scenario: Host switches environment before Start
- **WHEN** the host chooses another offered environment before pressing Start
- **THEN** the booking uses the newly chosen environment

### Requirement: The host accepts the terms before paying
The system SHALL show the terms of use, including what teams may and may not do with their VMs, and SHALL NOT take a host to payment until the host has accepted them. The acceptance SHALL be recorded with the booking.

#### Scenario: Terms not accepted
- **WHEN** a host tries to continue to payment without accepting the terms
- **THEN** the system does not start a payment
