-- Sessions keep their Proxmox ticket only in the browser's ticket cookie.
ALTER TABLE sessions
    DROP COLUMN pve_user,
    DROP COLUMN pve_ticket_sealed,
    DROP COLUMN pve_csrf_sealed,
    DROP COLUMN pve_issued_at,
    DROP COLUMN pve_login_at;
