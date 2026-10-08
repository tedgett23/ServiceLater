# cmdb

Configuration items: what the services, systems and devices are, how they depend on each other, and which tickets
are about them. See docs/apps-platform.md §8.2.

First-party ServiceLater app. Same structure and install path as any third-party app (see §11 of the MVP doc).

## What it ships

- **Classes** (`classes/`): business service, application, database, cloud resource, other, and hardware with
  server, computer, mobile device and network device extending it. A class has the fields of the class it extends
  first.
- **Relationship types** (`relationships/`): depends on / used by and runs on / runs, which carry impact (trouble
  with the second item affects the first); part of / contains and connected to, which do not.
- **Role** `cmdb-manager`: adds, changes and retires items. Agents can already read the CMDB and link items to
  tickets; admins can do everything.

## Customising

Add a class, or fields to a shipped one, in the local layer (`SL_APPS_LOCAL_DIR/cmdb/classes/`):

```yaml
# classes/load_balancer.yaml: a class of your own
kind: class
name: load_balancer
label: Load balancer
extends: network_device
fields:
  vip: { type: text, label: Virtual IP }
```

```yaml
# classes/hardware.yaml: one more field on every kind of hardware
fields:
  rack: { type: text, label: Rack }
```

A class may only extend one of this app's classes. Items whose class is removed stay, but their fields cannot be
changed until a class of that name is installed again.
