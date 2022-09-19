Container Security
=======================

references:
---------------
https://www.redhat.com/en/topics/security/container-security
https://www.vmware.com/topics/glossary/content/container-security.html
https://www.trendmicro.com/en_au/what-is/container-security.html
https://www.paloaltonetworks.com/cyberpedia/what-is-container-security
https://www.paloaltonetworks.com/prisma/cloud/cloud-workload-protection-platform/container-security
https://www.paloaltonetworks.com/network-security/cn-series

https://threatpost.com/container_threats_cloud_defend/179452/

Good read
https://cloudsecurityalliance.org/blog/2021/05/05/application-container-security-risks-and-countermeasures/
https://www.securitycompassadvisory.com/blog/confronting-common-container-security-vulnerabilities/
https://www.redhat.com/en/blog/four-container-and-kubernetes-security-risks-you-should-mitigate


https://sysdig.com/learn-cloud-native/container-security/what-is-container-security/
https://www.splunk.com/en_us/data-insider/what-is-container-security.html
https://www.spiceworks.com/it-security/vulnerability-management/articles/kubernetes-vulnerabilities-as-attack-vectors/

Best Practices
-----------------
* download images should be scanned and safe
* if possible build your own images in a safe secure environment
* do not trust upstream container images, have some safeguards in place to scan/check/verify images before deploying anywhere
* reduce container size to reduce the attack footprint
* make containers short lived
* decommission containers when not in use, check containers are not idle
* keep runtime engines up-to-date. perform regular maintenance
* ensure you have a full view into the container lifecycle, if you dont know you dont know. pipeline scanning, image registries, ci/cd
* Audit your environment regularly - user transactions, application flow, network status
* Use container priveledges, no root, read only volumes, limited packages, not package installer, network restirctions
* lock down host environment and rotate regularly
* monitor the network in real time
* monitor the API logs
* monitor the container logs
* 
