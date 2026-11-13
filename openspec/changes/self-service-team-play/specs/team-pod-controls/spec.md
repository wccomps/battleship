# Spec Delta

## Purpose

Gives a team in a running session the in-between level of access: it can power its own pod's VMs and roll them back to the baseline without staff, and can do nothing else to them and nothing at all to any other pod.

## ADDED Requirements

### Requirement: A team can power its own pod's VMs
The system SHALL let a team in a running session start, shut down, stop and reboot any VM in its own pod.

#### Scenario: Team reboots one of its VMs
- **WHEN** a team reboots a VM in its own pod while its session runs
- **THEN** the VM reboots

### Requirement: A team can reset its own pod's VMs to the baseline
The system SHALL let a team in a running session roll any VM in its own pod back to the baseline taken at deployment, without a ticket and without staff.

#### Scenario: Team resets a broken VM
- **WHEN** a team rolls one of its VMs back to the baseline while its session runs
- **THEN** the VM returns to its state at deployment and can be started

### Requirement: Control stops at the pod's edge
A team SHALL NOT be able to see, power, reset or open a console on a VM outside its own pod, including the pods of other teams in the same booking. The host's team SHALL have no control over sub-team pods.

#### Scenario: Team targets another pod in the same booking
- **WHEN** a team tries to power or reset a VM in another pod of the same booking
- **THEN** the request is refused and the VM is unchanged

#### Scenario: Host targets a sub-team's pod
- **WHEN** the host's team tries to power or reset a VM in a sub-team's pod
- **THEN** the request is refused and the VM is unchanged

### Requirement: From the jump box, a team reaches only its own pod
A session account on the jump box SHALL be able to connect to its own pod's VMs and to no other pod's, including the other pods of the same booking.

#### Scenario: Team connects to its own box
- **WHEN** pod 51's members open an SSH connection from the jump box to a VM in pod 51
- **THEN** the connection is made

#### Scenario: Team tries another pod from the jump box
- **WHEN** pod 51's members open a connection from the jump box to a VM in pod 52
- **THEN** the connection does not succeed

### Requirement: Power and reset are the only added rights
A team SHALL NOT be able to create or delete VMs, take or delete snapshots, change a VM's hardware, disks or network, or act on templates.

#### Scenario: Team tries to take a snapshot
- **WHEN** a team tries to take a new snapshot of one of its VMs
- **THEN** the request is refused

#### Scenario: Team tries to change a network card
- **WHEN** a team tries to move one of its VMs to another network
- **THEN** the request is refused

### Requirement: Control ends with the session
A team's power, reset and console rights SHALL stop working when its session ends, including for anyone still signed in.

#### Scenario: Still signed in at the end
- **WHEN** a team member who signed in during the session tries to power a VM after the session has ended
- **THEN** the request is refused
