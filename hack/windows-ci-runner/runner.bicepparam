using './runner.bicep'

// Persistent runner VM name — must be unique within the resource group DNS label
param vmName = 'vm-minikube-runner'

// Standard_D16s_v3: 16 vCPUs, 64 GiB RAM, supports nested virtualization for Hyper-V
param vmSize = 'Standard_D16s_v3'

param adminUsername = 'minikubeadmin'

param adminPassword = readEnvironmentVariable('RUNNER_AZ_VM_PASSWORD', '')
