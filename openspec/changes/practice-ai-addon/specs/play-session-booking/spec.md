# Spec Delta

## MODIFIED Requirements

### Requirement: Play window duration and pricing tiers
The system SHALL offer a 4-hour half-play window at $40 USD and an 8-hour full-play window at $80 USD, with an optional AI add-on at $10 USD. The price SHALL be computed by the system from the chosen window and add-on, and SHALL NOT depend on the number of sub-teams.

#### Scenario: Half play
- **WHEN** a host chooses a 4-hour window
- **THEN** the amount to pay is $40 USD

#### Scenario: Full play
- **WHEN** a host chooses an 8-hour window
- **THEN** the amount to pay is $80 USD

#### Scenario: Full play with the add-on
- **WHEN** a host chooses an 8-hour window with the AI add-on
- **THEN** the amount to pay is $90 USD

#### Scenario: A price sent by the browser is ignored
- **WHEN** a booking request carries an amount that differs from the system's price for its window and add-on
- **THEN** the system charges its own price
