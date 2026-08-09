[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string] $LibraryPath
)

$ErrorActionPreference = "Stop"
$dll = (Resolve-Path -LiteralPath $LibraryPath).Path
$source = @'
using System;
using System.Runtime.InteropServices;
public static class BlugeUsearchProbe {
    [DllImport("DLLPATH", CallingConvention = CallingConvention.Cdecl)]
    public static extern UInt32 bluge_usearch_abi_version();
    [DllImport("DLLPATH", CallingConvention = CallingConvention.Cdecl)]
    public static extern IntPtr bluge_usearch_hardware_acceleration_compiled();
    [DllImport("DLLPATH", CallingConvention = CallingConvention.Cdecl)]
    public static extern IntPtr bluge_usearch_hardware_acceleration_available();
}
'@.Replace("DLLPATH", $dll.Replace('\', '\\'))

Add-Type -TypeDefinition $source
$compiled = [Runtime.InteropServices.Marshal]::PtrToStringAnsi(
    [BlugeUsearchProbe]::bluge_usearch_hardware_acceleration_compiled())
$available = [Runtime.InteropServices.Marshal]::PtrToStringAnsi(
    [BlugeUsearchProbe]::bluge_usearch_hardware_acceleration_available())
@{
    abi_version = [BlugeUsearchProbe]::bluge_usearch_abi_version()
    compiled = $compiled
    available = $available
} | ConvertTo-Json -Compress
