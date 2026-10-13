package sandbox

// Hyper-V attaches a VHDX and nothing else — it will not take the sparse raw
// image every other machine is handed — so on Windows the sandbox's disks are
// dynamic VHDXs, which is the same property under another name: the file costs
// what has been written into it rather than what it could hold.
//
// The extension is here rather than inside the machine because it is not only
// the machine's business. Three separate things enumerate a workspace's disks:
// the machine that boots them, the reclaim that measures them, and the rebuild
// that deletes them so the environment is built again. A machine that used a
// name of its own would leave a disk the other two could not find, and
// "rebuild the environment" would quietly go on using the old one.
const diskExt = ".vhdx"

// diskFormat is what to call that format to a tool that reads several.
const diskFormat = "vhdx"

func makeDisk(path string, want int64) error { return virtualDisk(path, want) }
