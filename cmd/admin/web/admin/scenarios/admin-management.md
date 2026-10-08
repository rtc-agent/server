---
title: Admin Management
description: Cross-system workflows involving admin roles, permissions, and audit logs
tags: [admin, roles, permissions, audit]
---

# Admin Management

This system manages the administrator lifecycle through four subsystems:

1. **Admin Users** — administrator accounts and their attributes
2. **Roles** — permission bundles assigned to admin users
3. **Permissions** — fine-grained access control points (resource:action pairs)
4. **Audit Logs** — operation records for compliance and debugging

## Typical Workflows

### Onboard a new team
1. Create a role with the required permission set (e.g. "content-editor" with role:read)
2. Assign the role to the new admin user
3. Verify the user can access the intended pages

### Restructure access
1. Review current role assignments and audit logs to understand usage
2. Update roles — adjust permissions to match the new policy
3. Affected admin users inherit the changes immediately

### Investigate an incident
1. Navigate to audit logs to find the relevant operation
2. Identify which admin performed the action and when
3. Review the admin's role and permission assignments

## Permission Model

| Resource | Actions | Description |
|----------|---------|-------------|
| admin_user | read, write, delete | Admin account management |
| role | read, write | Role CRUD |
| permission | read, write | Permission definition |
| audit_log | read | Audit log access |
