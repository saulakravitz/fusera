// Copyright 2015 - 2017 Ka-Hing Cheung
// Copyright 2015 - 2017 Google Inc. All Rights Reserved.
// Modifications Copyright 2018 The MITRE Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fusera

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mattrbianchi/twig"
	"github.com/mitre/fusera/awsutil"
	"github.com/mitre/fusera/log"
	"github.com/mitre/fusera/nr"
	"github.com/pkg/errors"

	"github.com/sirupsen/logrus"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

type Options struct {
	Ngc []byte
	Acc map[string]bool
	Loc string
	// SRR# has a map of file names that map to urls where the data is
	Urls map[string]map[string]string

	// File system
	MountOptions      map[string]string
	MountPoint        string
	MountPointArg     string
	MountPointCreated string

	Cache    []string
	DirMode  os.FileMode
	FileMode os.FileMode
	Uid      uint32
	Gid      uint32

	// Tuning
	StatCacheTTL time.Duration
	TypeCacheTTL time.Duration

	// Debugging
	Debug      bool
	DebugFuse  bool
	DebugS3    bool
	Foreground bool
}

func Mount(ctx context.Context, opt *Options) (*Fusera, *fuse.MountedFileSystem, error) {
	fs, err := NewFusera(ctx, opt)
	if err != nil {
		return nil, nil, err
	}
	if fs == nil {
		return nil, nil, errors.New("Mount: initialization failed")
	}
	s := fuseutil.NewFileSystemServer(fs)
	fuseLog := log.GetLogger("fuse")
	mntConfig := &fuse.MountConfig{
		FSName:                  "fusera",
		ErrorLogger:             log.GetStdLogger(log.NewLogger("fuse"), logrus.ErrorLevel),
		DisableWritebackCaching: true,
	}
	if opt.DebugFuse {
		fuseLog.Level = logrus.DebugLevel
		log.Log.Level = logrus.DebugLevel
		mntConfig.DebugLogger = log.GetStdLogger(fuseLog, logrus.DebugLevel)
	}
	mfs, err := fuse.Mount(opt.MountPoint, s, mntConfig)
	if err != nil {
		return nil, nil, errors.Errorf("Mount: %v", err)
	}
	return fs, mfs, nil
}

func NewFusera(ctx context.Context, opt *Options) (*Fusera, error) {
	accessions, err := nr.ResolveNames(opt.Loc, opt.Ngc, opt.Acc)
	if err != nil {
		return nil, err
	}
	fs := &Fusera{
		accs:  accessions,
		opt:   opt,
		umask: 0122,
	}

	// if opt.DebugS3 {
	// 	awsConfig.LogLevel = aws.LogLevel(aws.LogDebug | aws.LogDebugWithRequestErrors)
	// 	s3Log.Level = logrus.DebugLevel
	// }

	now := time.Now()
	fs.rootAttrs = InodeAttributes{
		Size:  4096,
		Mtime: now,
	}

	fs.bufferPool = BufferPool{}.Init()

	fs.nextInodeID = fuseops.RootInodeID + 1
	fs.inodes = make(map[fuseops.InodeID]*Inode)
	root := NewInode(fs, nil, awsutil.String(""), awsutil.String(""))
	root.Id = fuseops.RootInodeID
	root.ToDir()
	root.Attributes.Mtime = fs.rootAttrs.Mtime

	fs.inodes[fuseops.RootInodeID] = root

	fs.nextHandleID = 1
	fs.dirHandles = make(map[fuseops.HandleID]*DirHandle)

	fs.fileHandles = make(map[fuseops.HandleID]*FileHandle)

	// fs.replicators = Ticket{Total: 16}.Init()
	// fs.restorers = Ticket{Total: 8}.Init()

	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 1000

	for id, acc := range accessions {
		// make directories here
		// dir
		//fmt.Println("making dir: ", accessions[i].ID)
		fullDirName := root.getChildName(id)
		root.mu.Lock()
		dir := NewInode(fs, root, awsutil.String(id), &fullDirName)
		dir.ToDir()
		dir.touch()
		root.mu.Unlock()
		fs.mu.Lock()
		fs.insertInode(root, dir)
		fs.mu.Unlock()
		// maybe do this?
		// dir.addDotAndDotDot()
		// put some files in the dirs
		for name, f := range acc.Files {
			//fmt.Println("making file: ", accessions[i].Files[j].Name)
			fullFileName := dir.getChildName(name)
			dir.mu.Lock()
			file := NewInode(fs, dir, awsutil.String(name), &fullFileName)
			// TODO: This will have to change when the real API is made
			file.Link = f.Link
			file.Acc = acc.ID
			u, err := strconv.ParseUint(f.Size, 10, 64)
			if err != nil {
				return nil, errors.New("failed to parse size into a uint64")
			}
			file.Attributes = InodeAttributes{
				Size:           u,
				Mtime:          f.ModifiedDate,
				ExpirationDate: f.ExpirationDate,
			}

			fh := NewFileHandle(file)
			fh.poolHandle = fs.bufferPool
			fh.buf = MBuf{}.Init(fh.poolHandle, 0, true)
			fh.dirty = true
			file.fileHandles = 1
			dir.touch()
			dir.mu.Unlock()
			fs.mu.Lock()
			// dir.insertChild(file)
			fs.insertInode(dir, file)
			hID := fs.nextHandleID
			fs.nextHandleID++
			fs.fileHandles[hID] = fh
			fs.mu.Unlock()

			// 	children: []fuseutil.Dirent{
			// 		fuseutil.Dirent{
			// 			Offset: 1,
			// 			Inode:  worldInode,
			// 			Name:   "world",
			// 			Type:   fuseutil.DT_File,
			// 		},
			// 	},
			// }
		}
	}

	return fs, nil
}

type Fusera struct {
	fuseutil.NotImplementedFileSystem

	// Fusera specific info
	accs map[string]nr.Accession

	opt *Options

	umask uint32

	rootAttrs InodeAttributes

	bufferPool *BufferPool

	// A lock protecting the state of the file system struct itself (distinct
	// from per-inode locks). Make sure to see the notes on lock ordering above.
	mu sync.Mutex

	// The next inode ID to hand out. We assume that this will never overflow,
	// since even if we were handing out inode IDs at 4 GHz, it would still take
	// over a century to do so.
	//
	// GUARDED_BY(mu)
	nextInodeID fuseops.InodeID

	// The collection of live inodes, keyed by inode ID. No ID less than
	// fuseops.RootInodeID is ever used.
	//
	// INVARIANT: For all keys k, fuseops.RootInodeID <= k < nextInodeID
	// INVARIANT: For all keys k, inodes[k].ID() == k
	// INVARIANT: inodes[fuseops.RootInodeID] is missing or of type inode.DirInode
	// INVARIANT: For all v, if IsDirName(v.Name()) then v is inode.DirInode
	//
	// GUARDED_BY(mu)
	inodes map[fuseops.InodeID]*Inode

	nextHandleID fuseops.HandleID
	dirHandles   map[fuseops.HandleID]*DirHandle

	fileHandles map[fuseops.HandleID]*FileHandle

	// replicators *Ticket
	// restorers   *Ticket

	forgotCnt uint32
}

func (fs *Fusera) allocateInodeId() (id fuseops.InodeID) {
	id = fs.nextInodeID
	fs.nextInodeID++
	return
}

func (fs *Fusera) SigUsr1() {
	fs.mu.Lock()

	twig.Infof("forgot %v inodes", fs.forgotCnt)
	twig.Infof("%v inodes", len(fs.inodes))
	fs.mu.Unlock()
	debug.FreeOSMemory()
}

// Find the given inode. Panic if it doesn't exist.
//
// LOCKS_REQUIRED(fs.mu)
func (fs *Fusera) getInodeOrDie(id fuseops.InodeID) (inode *Inode) {
	inode = fs.inodes[id]
	if inode == nil {
		panic(fmt.Sprintf("Unknown inode: %v", id))
	}

	return
}

func (fs *Fusera) StatFS(ctx context.Context, op *fuseops.StatFSOp) (err error) {
	//fmt.Println("sddp.go/StatFS called")

	const BLOCK_SIZE = 4096
	const TOTAL_SPACE = 1 * 1024 * 1024 * 1024 * 1024 * 1024 // 1PB
	const TOTAL_BLOCKS = TOTAL_SPACE / BLOCK_SIZE
	const INODES = 1 * 1000 * 1000 * 1000 // 1 billion
	op.BlockSize = BLOCK_SIZE
	op.Blocks = TOTAL_BLOCKS
	op.BlocksFree = TOTAL_BLOCKS
	op.BlocksAvailable = TOTAL_BLOCKS
	op.IoSize = 1 * 1024 * 1024 // 1MB
	op.Inodes = INODES
	op.InodesFree = INODES
	return
}

func (fs *Fusera) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) (err error) {
	//fmt.Println("sddp.go/GetInodeAttributes called")

	fs.mu.Lock()
	inode := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	attr, err := inode.GetAttributes()
	if err == nil {
		op.Attributes = *attr
		op.AttributesExpiration = time.Now().Add(fs.opt.StatCacheTTL)
	}

	return
}

func (fs *Fusera) GetXattr(ctx context.Context, op *fuseops.GetXattrOp) (err error) {
	//fmt.Println("sddp.go/GetXattr called")
	fs.mu.Lock()
	inode := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	value, err := inode.GetXattr(op.Name)
	if err != nil {
		return
	}

	op.BytesRead = len(value)

	if len(op.Dst) < op.BytesRead {
		return syscall.ERANGE
	} else {
		copy(op.Dst, value)
		return
	}
}

func (fs *Fusera) ListXattr(ctx context.Context, op *fuseops.ListXattrOp) (err error) {
	//fmt.Println("sddp.go/ListXattr called")
	fs.mu.Lock()
	inode := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	xattrs, err := inode.ListXattr()

	ncopied := 0

	for _, name := range xattrs {
		buf := op.Dst[ncopied:]
		nlen := len(name) + 1

		if nlen <= len(buf) {
			copy(buf, name)
			ncopied += nlen
			buf[nlen-1] = '\x00'
		}

		op.BytesRead += nlen
	}

	if ncopied < op.BytesRead {
		err = syscall.ERANGE
	}

	return
}

func (fs *Fusera) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) (err error) {
	//fmt.Println("sddp.go/LookUpInode called with:")
	//fmt.Println("op.Parent: ", op.Parent)
	//fmt.Println("op.Name: ", op.Name)

	var inode *Inode
	var ok bool
	defer func() { log.FuseLog.Debugf("<-- LookUpInode %v %v %v", op.Parent, op.Name, err) }()

	fs.mu.Lock()
	parent := fs.getInodeOrDie(op.Parent)
	fs.mu.Unlock()

	parent.mu.Lock()
	fs.mu.Lock()
	inode = parent.findChildUnlockedFull(op.Name)
	if inode != nil {
		ok = true
		inode.Ref()
	} else {
		ok = false
	}
	fs.mu.Unlock()
	parent.mu.Unlock()

	if !ok {
		return fuse.ENOENT
	}

	op.Entry.Child = inode.Id
	op.Entry.Attributes = inode.InflateAttributes()
	op.Entry.AttributesExpiration = time.Now().Add(fs.opt.StatCacheTTL)
	op.Entry.EntryExpiration = time.Now().Add(fs.opt.TypeCacheTTL)

	return
}

// LOCKS_REQUIRED(fs.mu)
// LOCKS_REQUIRED(parent.mu)
func (fs *Fusera) insertInode(parent *Inode, inode *Inode) {
	//fmt.Println("sddp.go/insertInode called")
	inode.Id = fs.allocateInodeId()
	parent.insertChildUnlocked(inode)
	fs.inodes[inode.Id] = inode
}

func (fs *Fusera) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) (err error) {
	//fmt.Println("sddp.go/OpenDir called with")
	//fmt.Println("op.Inode: ", op.Inode)
	fs.mu.Lock()

	handleID := fs.nextHandleID
	fs.nextHandleID++

	in := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	// XXX/is this a dir?
	dh := in.OpenDir()

	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.dirHandles[handleID] = dh
	op.Handle = handleID

	return
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *Fusera) insertInodeFromDirEntry(parent *Inode, entry *DirHandleEntry) (inode *Inode) {
	//fmt.Println("sddp.go/insertInodeFromDirEntry called")
	parent.mu.Lock()
	defer parent.mu.Unlock()

	inode = parent.findChildUnlocked(*entry.Name, entry.Type == fuseutil.DT_Directory)
	if inode == nil {
		path := parent.getChildName(*entry.Name)
		inode = NewInode(fs, parent, entry.Name, &path)
		if entry.Type == fuseutil.DT_Directory {
			inode.ToDir()
		} else {
			inode.Attributes = *entry.Attributes
		}
		if entry.ETag != nil {
			inode.s3Metadata["etag"] = []byte(*entry.ETag)
		}
		if entry.StorageClass != nil {
			inode.s3Metadata["storage-class"] = []byte(*entry.StorageClass)
		}
		// these are fake dir entries, we will realize the refcnt when
		// lookup is done
		inode.refcnt = 0

		fs.mu.Lock()
		defer fs.mu.Unlock()

		fs.insertInode(parent, inode)
	} else {
		inode.mu.Lock()
		defer inode.mu.Unlock()

		if entry.ETag != nil {
			inode.s3Metadata["etag"] = []byte(*entry.ETag)
		}
		if entry.StorageClass != nil {
			inode.s3Metadata["storage-class"] = []byte(*entry.StorageClass)
		}
		inode.KnownSize = &entry.Attributes.Size
		inode.Attributes.Mtime = entry.Attributes.Mtime
		inode.AttrTime = time.Now()
	}
	return
}

func makeDirEntry(en *DirHandleEntry) fuseutil.Dirent {
	//fmt.Println("sddp.go/makeDirEntry called with")
	//fmt.Println("en.Name: ", *en.Name)
	//fmt.Println("en.Type: ", en.Type)
	//fmt.Println("en.Offset: ", en.Offset)
	return fuseutil.Dirent{
		Name:   *en.Name,
		Type:   en.Type,
		Inode:  fuseops.RootInodeID + 1,
		Offset: en.Offset,
	}
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *Fusera) ReadDir(ctx context.Context, op *fuseops.ReadDirOp) (err error) {
	//fmt.Println("sddp.go/ReadDir called with")
	//fmt.Println("op.Handle: ", op.Handle)

	// Find the handle.
	fs.mu.Lock()
	dh := fs.dirHandles[op.Handle]
	fs.mu.Unlock()

	if dh == nil {
		panic(fmt.Sprintf("can't find dh=%v", op.Handle))
	}

	inode := dh.inode
	inode.logFuse("ReadDir", op.Offset)

	dh.mu.Lock()
	defer dh.mu.Unlock()

	readFromS3 := false

	for i := op.Offset; ; i++ {
		e, err := dh.ReadDir(i)
		if err != nil {
			return err
		}
		if e == nil {
			// we've reached the end, if this was read
			// from S3 then update the cache time
			if readFromS3 {
				inode.dir.DirTime = time.Now()
				inode.Attributes.Mtime = inode.findChildMaxTime()
			}
			break
		}

		if e.Inode == 0 {
			readFromS3 = true
			fs.insertInodeFromDirEntry(inode, e)
		}

		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], makeDirEntry(e))
		if n == 0 {
			break
		}

		dh.inode.logFuse("<-- ReadDir", *e.Name, e.Offset)

		op.BytesRead += n
	}

	return
}

func (fs *Fusera) ReleaseDirHandle(ctx context.Context, op *fuseops.ReleaseDirHandleOp) (err error) {
	//fmt.Println("sddp.go/ReleaseDirHandle called")

	fs.mu.Lock()
	defer fs.mu.Unlock()

	dh := fs.dirHandles[op.Handle]
	dh.CloseDir()

	log.FuseLog.Debugln("ReleaseDirHandle", *dh.inode.FullName())

	delete(fs.dirHandles, op.Handle)

	return
}

func (fs *Fusera) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) (err error) {
	//fmt.Println("sddp.go/OpenFile called")
	fs.mu.Lock()
	in := fs.getInodeOrDie(op.Inode)
	fs.mu.Unlock()

	fh, err := in.OpenFile()
	if err != nil {
		return
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	handleID := fs.nextHandleID
	fs.nextHandleID++

	fs.fileHandles[handleID] = fh

	op.Handle = handleID
	op.KeepPageCache = true

	return
}

func (fs *Fusera) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) (err error) {
	//fmt.Println("sddp.go/ReadFile called")

	fs.mu.Lock()
	fh := fs.fileHandles[op.Handle]
	fs.mu.Unlock()

	op.BytesRead, err = fh.ReadFile(op.Offset, op.Dst)

	return
}

func (fs *Fusera) SyncFile(ctx context.Context, op *fuseops.SyncFileOp) (err error) {

	// intentionally ignored, so that write()/sync()/write() works
	// see https://github.com/kahing/goofys/issues/154
	return
}

func (fs *Fusera) ReleaseFileHandle(ctx context.Context, op *fuseops.ReleaseFileHandleOp) (err error) {
	//fmt.Println("sddp.go/ReleaseFileHandle called")
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fh := fs.fileHandles[op.Handle]
	fh.Release()

	log.FuseLog.Debugln("ReleaseFileHandle", *fh.inode.FullName())

	delete(fs.fileHandles, op.Handle)

	// try to compact heap
	//fs.bufferPool.MaybeGC()
	return
}
