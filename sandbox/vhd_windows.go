package sandbox

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The sandbox's disks, in the format Hyper-V will attach.
//
// Every other machine takes a sparse raw file: QEMU is told format=raw and
// Virtualization.framework attaches whatever it is handed. Hyper-V will not.
// A SCSI attachment is a VHD or a VHDX and nothing else, so the two disks
// machine.go describes — the sandbox's own state, and the workspace's shared
// environment — are made here as dynamic VHDX files beside the raw paths the
// rest of the package names.
//
// Dynamic, which is the VHDX equivalent of sparse: the file costs what has
// been written into it rather than what it could hold, so the hundred
// gigabytes a developer's sandbox is allowed is a limit rather than an
// allocation. That is the same property stateDisk relies on, kept.
//
// virtdisk.dll rather than anything of our own. A fixed VHD is raw bytes and a
// footer and could have been written by hand, but a dynamic VHDX is a real
// format with a log and a block allocation table, and the operating system
// ships the code that writes it.

var (
	virtdisk = windows.NewLazySystemDLL("virtdisk.dll")

	procCreateVirtualDisk         = virtdisk.NewProc("CreateVirtualDisk")
	procOpenVirtualDisk           = virtdisk.NewProc("OpenVirtualDisk")
	procExpandVirtualDisk         = virtdisk.NewProc("ExpandVirtualDisk")
	procGetVirtualDiskInformation = virtdisk.NewProc("GetVirtualDiskInformation")
)

const (
	virtualStorageTypeDeviceVHDX = 3
	createVirtualDiskVersion2    = 2
	openVirtualDiskVersion2      = 2
	expandVirtualDiskVersion1    = 1
	// GET_VIRTUAL_DISK_INFO_SIZE.
	getVirtualDiskInfoSize = 1
)

// msftVendor is the vendor GUID every Microsoft disk format carries.
var msftVendor = windows.GUID{
	Data1: 0xec984aec, Data2: 0xa0f9, Data3: 0x47e9,
	Data4: [8]byte{0x90, 0x1f, 0x71, 0x41, 0x5a, 0x66, 0x34, 0x5b},
}

type virtualStorageType struct {
	DeviceID uint32
	VendorID windows.GUID
}

// createVirtualDiskParameters is CREATE_VIRTUAL_DISK_PARAMETERS with its
// Version2 arm spelled out. The union begins at offset 8 because it holds
// pointers, which is what the padding after Version is for.
type createVirtualDiskParameters struct {
	Version uint32
	_       uint32

	UniqueID                  windows.GUID
	MaximumSize               uint64
	BlockSizeInBytes          uint32
	SectorSizeInBytes         uint32
	PhysicalSectorSizeInBytes uint32
	_                         uint32
	ParentPath                *uint16
	SourcePath                *uint16
	OpenFlags                 uint32
	ParentVirtualStorageType  virtualStorageType
	SourceVirtualStorageType  virtualStorageType
	ResiliencyGUID            windows.GUID
}

// openVirtualDiskParameters is OPEN_VIRTUAL_DISK_PARAMETERS with its Version2
// arm spelled out. Version1 is the wrong one here and fails in a way that only
// shows on a sandbox's second boot: its single field is RWDepth, which has to
// be at least one and means nothing for a VHDX, so the call answers "the
// parameter is incorrect" for a disk that is perfectly good. Every member of
// this union is four-byte aligned, so unlike the create parameters next door
// there is no padding after Version.
type openVirtualDiskParameters struct {
	Version        uint32
	GetInfoOnly    int32
	ReadOnly       int32
	ResiliencyGUID windows.GUID
}

type expandVirtualDiskParameters struct {
	Version uint32
	_       uint32
	NewSize uint64
}

// getVirtualDiskInfoSizeResult is GET_VIRTUAL_DISK_INFO with its Size arm
// spelled out. The trailing padding is the union's other arms, which carry
// GUIDs and path buffers: the call is handed the size of the whole structure
// and refuses a buffer that could not have held any of them.
type getVirtualDiskInfoSizeResult struct {
	Version      uint32
	_            uint32
	VirtualSize  uint64
	PhysicalSize uint64
	BlockSize    uint32
	SectorSize   uint32
	_            [1024]byte
}

// virtualDisk makes a dynamic VHDX of at least want bytes at path, growing one
// that is already there and never shrinking it.
//
// It is the Hyper-V counterpart of sparseDisk in machine.go, and it keeps that
// function's one rule: a disk is only ever grown. Shrinking one out from under
// a sandbox that has written to it would lose the work the disk is kept for,
// and the filesystem on it would not survive being cut short either.
func virtualDisk(path string, want int64) error {
	if err := virtdisk.Load(); err != nil {
		return fmt.Errorf("sandbox: virtdisk.dll: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return expandVirtualDiskTo(path, want)
	} else if !os.IsNotExist(err) {
		return err
	}

	wpath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	id, err := windows.GenerateGUID()
	if err != nil {
		return err
	}
	storage := virtualStorageType{DeviceID: virtualStorageTypeDeviceVHDX, VendorID: msftVendor}
	params := createVirtualDiskParameters{
		Version:     createVirtualDiskVersion2,
		UniqueID:    id,
		MaximumSize: uint64(want),
		// Zero asks virtdisk for the format's own defaults, which for VHDX is
		// a 32 MB block and a 512-byte logical sector. Naming them here would
		// be pinning numbers we have no reason to hold an opinion about.
		BlockSizeInBytes:          0,
		SectorSizeInBytes:         0,
		PhysicalSectorSizeInBytes: 0,
	}
	var handle windows.Handle
	// VIRTUAL_DISK_ACCESS_NONE, which is what a Version2 create takes: the
	// access mask moved into the parameters in that version.
	hr, _, _ := procCreateVirtualDisk.Call(
		uintptr(unsafe.Pointer(&storage)),
		uintptr(unsafe.Pointer(wpath)),
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(&params)),
		0,
		uintptr(unsafe.Pointer(&handle)),
	)
	if uint32(hr) != 0 {
		return fmt.Errorf("sandbox: creating %s: %w", path, windows.Errno(hr))
	}
	windows.CloseHandle(handle)
	return nil
}

// expandVirtualDiskTo grows an existing disk, and says nothing when it is
// already big enough — which is every boot after the first.
func expandVirtualDiskTo(path string, want int64) error {
	wpath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	storage := virtualStorageType{DeviceID: virtualStorageTypeDeviceVHDX, VendorID: msftVendor}
	open := openVirtualDiskParameters{Version: openVirtualDiskVersion2}
	var handle windows.Handle
	// VIRTUAL_DISK_ACCESS_NONE, which a Version2 open requires: from that
	// version on the access is taken from the parameters — neither GetInfoOnly
	// nor ReadOnly, so this is a handle that can read the size and change it.
	hr, _, _ := procOpenVirtualDisk.Call(
		uintptr(unsafe.Pointer(&storage)),
		uintptr(unsafe.Pointer(wpath)),
		0,
		0,
		uintptr(unsafe.Pointer(&open)),
		uintptr(unsafe.Pointer(&handle)),
	)
	if uint32(hr) != 0 {
		return fmt.Errorf("sandbox: opening %s: %w", path, windows.Errno(hr))
	}
	defer windows.CloseHandle(handle)

	info := getVirtualDiskInfoSizeResult{Version: getVirtualDiskInfoSize}
	size := uint32(unsafe.Sizeof(info))
	var used uint32
	hr, _, _ = procGetVirtualDiskInformation.Call(uintptr(handle),
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&used)))
	if uint32(hr) != 0 {
		return fmt.Errorf("sandbox: reading the size of %s: %w", path, windows.Errno(hr))
	}
	if info.VirtualSize >= uint64(want) {
		return nil
	}

	expand := expandVirtualDiskParameters{Version: expandVirtualDiskVersion1, NewSize: uint64(want)}
	hr, _, _ = procExpandVirtualDisk.Call(uintptr(handle), 0,
		uintptr(unsafe.Pointer(&expand)), 0)
	if uint32(hr) != 0 {
		return fmt.Errorf("sandbox: growing %s to %d bytes: %w", path, want, windows.Errno(hr))
	}
	return nil
}
