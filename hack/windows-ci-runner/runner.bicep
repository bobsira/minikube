// runner.bicep - Persistent Azure VM for self-hosted GitHub Actions Windows Hyper-V runner
// This VM is long-lived (not ephemeral per test run) and runs the GitHub Actions runner service.
// VM size Standard_D16s_v3 supports nested virtualization required for Hyper-V testing.
//
// Uses a marketplace Windows Server 2022 image so no Shared Image Gallery access is required.
// Switch to the SIG-based image (see functional_extra.yml) once minikube subscription access is granted.
targetScope = 'resourceGroup'

param vmName string
param vmSize string = 'Standard_D16s_v3'
param adminUsername string = 'minikubeadmin'
@secure()
param adminPassword string

var location = resourceGroup().location
var nameSuffix = uniqueString(resourceGroup().id, vmName)
var networkInterfaceName = '${vmName}-nic-${nameSuffix}'
var networkSecurityGroupName = '${vmName}-nsg-${nameSuffix}'
var publicIpName = '${vmName}-pip-${nameSuffix}'
var subnetName = '${vmName}-snet-${nameSuffix}'
var virtualNetworkName = '${vmName}-vnet-${nameSuffix}'

resource virtualNetwork 'Microsoft.Network/virtualNetworks@2023-11-01' = {
  name: virtualNetworkName
  location: location
  tags: { project: 'minikube', purpose: 'ci-runner' }
  properties: {
    addressSpace: {
      addressPrefixes: ['10.0.0.0/16']
    }
    subnets: [
      {
        name: subnetName
        properties: {
          addressPrefix: '10.0.1.0/24'
          networkSecurityGroup: { id: networkSecurityGroup.id }
        }
      }
    ]
  }
}

resource publicIp 'Microsoft.Network/publicIPAddresses@2023-11-01' = {
  name: publicIpName
  location: location
  tags: { project: 'minikube', purpose: 'ci-runner' }
  sku: { name: 'Standard' }
  properties: {
    dnsSettings: { domainNameLabel: vmName }
    publicIPAddressVersion: 'IPv4'
    publicIPAllocationMethod: 'Static'
  }
}

resource networkSecurityGroup 'Microsoft.Network/networkSecurityGroups@2023-11-01' = {
  name: networkSecurityGroupName
  location: location
  tags: { project: 'minikube', purpose: 'ci-runner' }
  properties: {
    securityRules: [
      {
        name: 'AllowRDP'
        properties: {
          access: 'Allow'
          destinationAddressPrefix: '*'
          destinationPortRange: '3389'
          direction: 'Inbound'
          priority: 1000
          protocol: 'Tcp'
          sourceAddressPrefix: '*'
          sourcePortRange: '*'
        }
      }
      {
        name: 'AllowSSH'
        properties: {
          access: 'Allow'
          destinationAddressPrefix: '*'
          destinationPortRange: '22'
          direction: 'Inbound'
          priority: 1001
          protocol: 'Tcp'
          sourceAddressPrefix: '*'
          sourcePortRange: '*'
        }
      }
    ]
  }
}

resource networkInterface 'Microsoft.Network/networkInterfaces@2023-11-01' = {
  name: networkInterfaceName
  location: location
  tags: { project: 'minikube', purpose: 'ci-runner' }
  properties: {
    ipConfigurations: [
      {
        name: 'internal'
        properties: {
          subnet: { id: '${virtualNetwork.id}/subnets/${subnetName}' }
          privateIPAllocationMethod: 'Dynamic'
          publicIPAddress: { id: publicIp.id }
        }
      }
    ]
    networkSecurityGroup: { id: networkSecurityGroup.id }
  }
}

resource virtualMachine 'Microsoft.Compute/virtualMachines@2023-09-01' = {
  name: vmName
  location: location
  tags: { project: 'minikube', purpose: 'ci-runner' }
  properties: {
    hardwareProfile: { vmSize: vmSize }
    osProfile: {
      computerName: take(vmName, 15)
      adminUsername: adminUsername
      adminPassword: adminPassword
      windowsConfiguration: {
        enableAutomaticUpdates: false
        patchSettings: { patchMode: 'Manual' }
      }
    }
    storageProfile: {
      // Windows Server 2022 Azure Edition Gen 2 — supports nested virtualization on Dv3/Ev3 sizes.
      // TODO: Switch to SIG minikube-ci-windows-11 once minikube subscription access is granted.
      imageReference: {
        publisher: 'MicrosoftWindowsServer'
        offer: 'WindowsServer'
        sku: '2022-datacenter-azure-edition'
        version: 'latest'
      }
      osDisk: {
        createOption: 'FromImage'
        managedDisk: { storageAccountType: 'StandardSSD_LRS' }
        deleteOption: 'Detach'
      }
    }
    networkProfile: {
      networkInterfaces: [
        {
          id: networkInterface.id
          properties: { primary: true }
        }
      ]
    }
  }
}

// Enable OpenSSH server so the provisioning workflow can connect via SSH/SCP.
// Hyper-V is enabled separately in the provisioning workflow (requires a reboot).
resource enableSsh 'Microsoft.Compute/virtualMachines/extensions@2023-09-01' = {
  name: 'EnableOpenSSH'
  parent: virtualMachine
  location: location
  properties: {
    publisher: 'Microsoft.Compute'
    type: 'CustomScriptExtension'
    typeHandlerVersion: '1.10'
    autoUpgradeMinorVersion: true
    settings: {
      commandToExecute: 'powershell -ExecutionPolicy Bypass -Command "Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0; Start-Service sshd; Set-Service -Name sshd -StartupType Automatic"'
    }
  }
}

// Auto-shutdown at midnight UTC to save costs during development.
// The VM can be started manually or via the provisioning workflow when needed.
resource autoShutdown 'Microsoft.DevTestLab/schedules@2018-09-15' = {
  name: 'shutdown-computevm-${vmName}'
  location: location
  tags: { project: 'minikube', purpose: 'ci-runner' }
  properties: {
    status: 'Enabled'
    taskType: 'ComputeVmShutdownTask'
    dailyRecurrence: { time: '0000' }
    timeZoneId: 'UTC'
    targetResourceId: virtualMachine.id
  }
}

output vmId string = virtualMachine.id
output vmName string = virtualMachine.name
output hostname string = publicIp.properties.dnsSettings.fqdn
output publicIpAddress string = publicIp.properties.ipAddress
