# OPanel Enterprise WHMCS Server Module

Compatible with WHMCS on PHP 8.1+.

## Install

Copy this directory to WHMCS:

```
modules/servers/opanelent/
```

## OPanel Enterprise API token

In OPanel Enterprise admin, create a provisioning API token with scopes:

```
provisioning:read,provisioning:write
```

## WHMCS server config

Go to **System Settings → Servers → Add New Server**.

| Field | Value |
|---|---|
| Module | OPanel Enterprise Hosting |
| Hostname | panel domain or IP |
| IP Address | optional |
| Assigned IP Addresses | empty |
| NS fields | empty |
| Type | OPanel Enterprise Hosting |
| Username | empty |
| Password | OPanel Enterprise API token, if not using Access Hash |
| Access Hash | OPanel Enterprise API token |
| Secure | checked if HTTPS |
| Port | OPanel Enterprise port, usually `2222` |

## Product config

Go to **System Settings → Products/Services → Module Settings**.

| Option | Example |
|---|---|
| Package | select an OPanel Enterprise package |
| App Type | `php` |
| PHP Version | `8.4` |
| Install WordPress | unchecked by default |
| Auto SSL | unchecked by default |
