# k8s-tools

## Reference Links

* awesome-docker tool list on github
* https://github.com/veggiemonk/awesome-docker/blob/master/README.md#terminal


k8s Security Tooling
=======================
1. Open Policy Agent (OPA): cluster policies : 

** https://github.com/open-policy-agent/opa

2. KubeLinter: yaml linter : 

** https://www.redhat.com/en/topics/containers/what-is-kubelinter

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

11. snyk io 

** https://snyk.io/
** https://support.snyk.io/hc/en-us/articles/360003946917-Test-images-with-the-Snyk-Container-CLI

12. anchore 

** https://docs.anchore.com/current/

13. aquasec 

** https://www.aquasec.com/

14. kubei 

** https://github.com/Portshift/kubei

15. Palo Alto twsitcli

** Scan images with twistcli - Palo Alto Networkshttps://docs.paloaltonetworks.com › prisma-cloud › tools

16. sysdig

** https://sysdig.com/products/secure/

17. kubesec

** https://github.com/controlplaneio/kubesec/releases

18. kubehunter - aquasec

** https://github.com/aquasecurity/kube-hunter

19. kdave

20. kube-bench - aquasec

** https://github.com/aquasecurity/kube-bench

21. kubeaudit

22.  Trivy Operator/CRD - vuln scan, audit and reporting to prom or other, argo integration

** https://github.com/aquasecurity/trivy-operator


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
1. admiralty
* https://admiralty.io/pricing
2. shipper
* https://github.com/bookingcom/shipper
3. kubfed
* https://github.com/kubernetes-sigs/kubefed
4. Rancher
* Rancher
5. Fleet
* Fleet is a GitOps-at-scale project designed to facilitate and manage a multi-cluster environment.
6. Google Anthos
* Google Anthos is designed to extend the Google Kubernetes engine across hybrid and multi-cluster environments.
7. Das shift engine
* https://github.com/telekom/das-schiff
8. https://rafay.co/ - governance and automation
9. https://www.paralus.io/ - policy management - access mgt, sso, rbac, auditing, zerotrust just in time accouting
10. https://blog.kubernauts.io/deploy-k8s-using-k8s-with-cluster-api-and-capa-on-aws-107669808367
11. CAPI
12. CAPA


k8s linters
============
https://github.com/projectatomic/dockerfile_lint

k8s logs
=============

1. stern/stern formerly known as wercker/stern
* https://github.com/stern/stern
