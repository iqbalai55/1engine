# 1engine

The platform rents GPU capacity to customers and settles usage against prepaid credits.

## Language

**Tenant**:
A customer account that owns one dstack project and one credit balance.
_Avoid_: Account, organization, user

**Run**:
A dstack workload submitted by a tenant. A run contains one or more jobs.
_Avoid_: Job, workload, instance

**Job**:
One container scheduled on one instance for a run.
_Avoid_: Task, replica, pod

**Container**:
The isolation boundary a customer rents. The customer has root inside it and no authority
outside it.
_Avoid_: Instance, VM, machine

**Host**:
The virtual machine or server that runs a container. It belongs to the operator.
_Avoid_: Server, node, machine

**SSH proxy**:
The single entry point for customer SSH. It authenticates the customer's public key and opens
the run container with the platform key.
_Avoid_: Bastion, gateway, jump host

**Customer SSH key**:
A public key a customer registers for run access. The private half never leaves the customer.
_Avoid_: Access key, API key

**Proxied SSH command**:
The SSH command dstack builds for a running job when the SSH proxy is enabled.
_Avoid_: Attach command, host command
