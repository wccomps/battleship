-- Battleship no longer reads the identity provider's groups.
ALTER TABLE sessions DROP COLUMN groups;
