# k8s-tools

## Reference Links



k8s Security Tooling
=======================
1. Open Policy Agent (OPA): cluster policies : https://github.com/open-policy-agent/opa

2. KubeLinter: yaml linter : https://www.redhat.com/en/topics/containers/what-is-kubelinter

3. Kube-bench - configuration scanner

** https://github.com/aquasecurity/kube-bench

4. Kube-hunter - Testing tool & pentesting

** https://github.com/aquasecurity/kube-hunter

5. Terrascan - static code analyzer - compliance & security for terraform/yaml,kustomize,docker

** https://github.com/accurics/terrascan 

6. Falco - pod or node or both

** https://falco.org/

7. Clair - container static analyzer

** https://github.com/quay/clair

8. Checkov - scan IaC

** https://www.checkov.io/

9. Sandfly security - node security

** https://www.sandflysecurity.com/get-sandfly/

10. Trivvy - container scanning

** https://github.com/aquasecurity/trivy

11. https://snyk.io/

** https://support.snyk.io/hc/en-us/articles/360003946917-Test-images-with-the-Snyk-Container-CLI

12. https://docs.anchore.com/current/

13. https://www.aquasec.com/

14. https://github.com/Portshift/kubei

kustomize
==============
ref link:https://www.openanalytics.eu/blog/2021/02/23/kustomize-best-practices/

### kustomize secrets
* gpg: user multi file
* SOPS: https://github.com/mozilla/sops
* vault hashicorp: https://github.com/benmorehouse/kustomize-vault
* * https://learn.hashicorp.com/tutorials/vault/kubernetes-sidecar


Multi-Cluster mgt
=============
* https://admiralty.io/pricing
* https://github.com/bookingcom/shipper
* https://github.com/kubernetes-sigs/kubefed
* Rancher
* Fleet is a GitOps-at-scale project designed to facilitate and manage a multi-cluster environment.
* Google Anthos is designed to extend the Google Kubernetes engine across hybrid and multi-cluster environments.


k8s linters
============
https://github.com/projectatomic/dockerfile_lint
