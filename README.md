# Rollout Injecting Webhook

A [Kubernetes Mutating Webhook](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/) Server, that injects an [Argo Rollout](https://argo-rollouts.readthedocs.io/en/stable/) for every [Deployment](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/). The rollout will take the configuration of the deployment and automatically enables a [Blue Green Strategy](https://argo-rollouts.readthedocs.io/en/stable/features/bluegreen/) for them. 

## Setup

The current (alpha) version does not support any specific configuration and just acts on every deployment in the given namespace. It scales the original deployment down to 0 and creates the pods through a rollout. The service from the original one is left in place and a preview-service is created, using a "-preview" post-fix. 
All objects created are annotated with ```"argocd.argoproj.io/compare-options" = "IgnoreExtraneous"``` and  ```"argocd.argoproj.io/sync-options" = "Prune=false"``` to not interfer with ArgoCD. 

Deployment happens, following the standard procedure of a Webhook-Deployment.

1. Create a certificate to be used by the webhook. This could be a self-signed(see [issuer](./example/self-signed-issuer.yaml) and [certificate](./example/certificate.yml)) certificate, issued by [cert-manager](https://cert-manager.io/).
2. Since the webhook needs to access the kubernetes APIs, a proper role has to be provided. It needs CRUD-Access to "Services" and "argoproj.io/Rollouts", thus a a service-account with such permissions needs to be created. See the [cluster-role](./example/cluster-role.yaml), [role-binding](./example/role-binding.yaml) and [service-account](./example/service-account.yaml) on how to create such.
3. The server needs to be deployed. Create a simple deployment, mounting the certificate to ```/etc/certs``` and a service, offering the 443 endpoint to the cluster. See [deployment](./example/deployment.yaml) and [service](./example/service.yaml). Be aware that the injecting webhook acts namespaced, e.g. it needs to be deployed to the namespace it should inject the rollouts. 
4. Create the webhook. The webhook also needs to be added to the namespace(namespaced webhooks limit the blast-radius in case of errors) and needs to get the CA of the certificate provided. If the certificate was created via cert-manager, this could be done via annotaiton. See the [mutating-webhook](./example/mutating-webhook.yaml)

## Excluding a deployment

In order to exclude a deployment from beeing replaced by a rollout, just add the annotation:
```yaml
metadata:
  name: webhook-server
  annotations:
    wistefan/rollout-injecting-webhook: ignore
```
