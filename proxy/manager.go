package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fosrl/newt/logger"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const (
	errUnsupportedProtoFmt = "unsupported protocol: %s"
	maxUDPPacketSize       = 65507 // Maximum UDP packet size
	defaultUDPIdleTimeout  = 90 * time.Second
)

// udpBufferPool provides reusable buffers for UDP packet handling.
// This reduces GC pressure from frequent large allocations.
var udpBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, maxUDPPacketSize)
		return &buf
	},
}

// getUDPBuffer retrieves a buffer from the pool.
func getUDPBuffer() *[]byte {
	return udpBufferPool.Get().(*[]byte)
}

// putUDPBuffer clears and returns a buffer to the pool.
func putUDPBuffer(buf *[]byte) {
	// Clear the buffer to prevent data leakage
	clear(*buf)
	udpBufferPool.Put(buf)
}

// Target represents a proxy target with its address and port
type Target struct {
	Address string
	Port    int
}

// managedListener wraps a net.Listener so an intentional Close() can be
// detected reliably by the accept loop. gVisor's netstack (unlike the
// stdlib) does not return net.ErrClosed from Accept() after Close() - it
// returns a generic "endpoint is in invalid state" error - so relying on
// errors.Is(err, net.ErrClosed) leaves the accept loop spinning forever.
type managedListener struct {
	net.Listener
	closed chan struct{}
}

func newManagedListener(l net.Listener) *managedListener {
	return &managedListener{Listener: l, closed: make(chan struct{})}
}

func (m *managedListener) Close() error {
	err := m.Listener.Close()
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return err
}

// managedPacketConn is the net.PacketConn equivalent of managedListener.
type managedPacketConn struct {
	net.PacketConn
	closed chan struct{}
}

func newManagedPacketConn(c net.PacketConn) *managedPacketConn {
	return &managedPacketConn{PacketConn: c, closed: make(chan struct{})}
}

func (m *managedPacketConn) Close() error {
	err := m.PacketConn.Close()
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return err
}

// ProxyManager handles the creation and management of proxy connections
type ProxyManager struct {
	tnet           *netstack.Net
	tcpTargets     map[string]map[int]string // map[listenIP]map[port]targetAddress
	udpTargets     map[string]map[int]string
	listeners      []net.Listener
	udpConns       []net.PacketConn
	running        bool
	mutex          sync.RWMutex
	nativeListenIP string // when non-empty, use native OS listeners instead of netstack
	udpIdleTimeout time.Duration

	// connection blocking
	blocked atomic.Bool
}

// NewProxyManager creates a new proxy manager instance backed by a netstack.
func NewProxyManager(tnet *netstack.Net) *ProxyManager {
	return &ProxyManager{
		tnet:           tnet,
		tcpTargets:     make(map[string]map[int]string),
		udpTargets:     make(map[string]map[int]string),
		listeners:      make([]net.Listener, 0),
		udpConns:       make([]net.PacketConn, 0),
		udpIdleTimeout: defaultUDPIdleTimeout,
	}
}

// NewProxyManagerNative creates a proxy manager that binds listeners directly
// to the host network stack on the given IP address.
func NewProxyManagerNative(listenIP string) *ProxyManager {
	return &ProxyManager{
		nativeListenIP: listenIP,
		tcpTargets:     make(map[string]map[int]string),
		udpTargets:     make(map[string]map[int]string),
		listeners:      make([]net.Listener, 0),
		udpConns:       make([]net.PacketConn, 0),
		udpIdleTimeout: defaultUDPIdleTimeout,
	}
}

// NewProxyManagerWithoutTNet creates a proxy manager with no backing network.
// Call SetTNet before starting.
func NewProxyManagerWithoutTNet() *ProxyManager {
	return &ProxyManager{
		tcpTargets:     make(map[string]map[int]string),
		udpTargets:     make(map[string]map[int]string),
		listeners:      make([]net.Listener, 0),
		udpConns:       make([]net.PacketConn, 0),
		udpIdleTimeout: defaultUDPIdleTimeout,
	}
}

// Function to add tnet to existing ProxyManager
func (pm *ProxyManager) SetTNet(tnet *netstack.Net) {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	pm.tnet = tnet
}

// SetBlocked enables or disables connection blocking.
// When enabled, all new incoming TCP connections are immediately closed
// and all incoming UDP packets are silently dropped.
func (pm *ProxyManager) SetBlocked(v bool) {
	pm.blocked.Store(v)
	if v {
		logger.Debug("ProxyManager: connection blocking enabled, new connections will be dropped")
	} else {
		logger.Debug("ProxyManager: connection blocking disabled, accepting connections")
	}
}

// AddTarget adds as new target for proxying
func (pm *ProxyManager) AddTarget(proto, listenIP string, port int, targetAddr string) error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	switch proto {
	case "tcp":
		if pm.tcpTargets[listenIP] == nil {
			pm.tcpTargets[listenIP] = make(map[int]string)
		}
		pm.tcpTargets[listenIP][port] = targetAddr
	case "udp":
		if pm.udpTargets[listenIP] == nil {
			pm.udpTargets[listenIP] = make(map[int]string)
		}
		pm.udpTargets[listenIP][port] = targetAddr
	default:
		return fmt.Errorf(errUnsupportedProtoFmt, proto)
	}

	if pm.running {
		return pm.startTarget(proto, listenIP, port, targetAddr)
	} else {
		logger.Debug("Not adding target because not running")
	}
	return nil
}

func (pm *ProxyManager) RemoveTarget(proto, listenIP string, port int) error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	switch proto {
	case "tcp":
		if targets, ok := pm.tcpTargets[listenIP]; ok {
			delete(targets, port)
			// Remove and close the corresponding TCP listener
			for i, listener := range pm.listeners {
				if addr, ok := listener.Addr().(*net.TCPAddr); ok && addr.Port == port {
					listener.Close()
					time.Sleep(50 * time.Millisecond)
					// Remove from slice
					pm.listeners = append(pm.listeners[:i], pm.listeners[i+1:]...)
					break
				}
			}
		} else {
			return fmt.Errorf("target not found: %s:%d", listenIP, port)
		}
	case "udp":
		if targets, ok := pm.udpTargets[listenIP]; ok {
			delete(targets, port)
			// Remove and close the corresponding UDP connection
			for i, conn := range pm.udpConns {
				if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.Port == port {
					conn.Close()
					time.Sleep(50 * time.Millisecond)
					// Remove from slice
					pm.udpConns = append(pm.udpConns[:i], pm.udpConns[i+1:]...)
					break
				}
			}
		} else {
			return fmt.Errorf("target not found: %s:%d", listenIP, port)
		}
	default:
		return fmt.Errorf(errUnsupportedProtoFmt, proto)
	}
	return nil
}

// Start begins listening for all configured proxy targets
func (pm *ProxyManager) Start() error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	if pm.running {
		return nil
	}

	// Start TCP targets
	for listenIP, targets := range pm.tcpTargets {
		for port, targetAddr := range targets {
			if err := pm.startTarget("tcp", listenIP, port, targetAddr); err != nil {
				return fmt.Errorf("failed to start TCP target: %v", err)
			}
		}
	}

	// Start UDP targets
	for listenIP, targets := range pm.udpTargets {
		for port, targetAddr := range targets {
			if err := pm.startTarget("udp", listenIP, port, targetAddr); err != nil {
				return fmt.Errorf("failed to start UDP target: %v", err)
			}
		}
	}

	pm.running = true
	return nil
}

// SetUDPIdleTimeout configures when idle UDP client flows are reclaimed.
func (pm *ProxyManager) SetUDPIdleTimeout(d time.Duration) {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()
	if d <= 0 {
		pm.udpIdleTimeout = defaultUDPIdleTimeout
		return
	}
	pm.udpIdleTimeout = d
}

func (pm *ProxyManager) Stop() error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	if !pm.running {
		return nil
	}

	// Set running to false first to signal handlers to stop
	pm.running = false

	// Close TCP listeners
	for i := len(pm.listeners) - 1; i >= 0; i-- {
		listener := pm.listeners[i]
		if err := listener.Close(); err != nil {
			logger.Error("Error closing TCP listener: %v", err)
		}
		// Remove from slice
		pm.listeners = append(pm.listeners[:i], pm.listeners[i+1:]...)
	}

	// Close UDP connections
	for i := len(pm.udpConns) - 1; i >= 0; i-- {
		conn := pm.udpConns[i]
		if err := conn.Close(); err != nil {
			logger.Error("Error closing UDP connection: %v", err)
		}
		// Remove from slice
		pm.udpConns = append(pm.udpConns[:i], pm.udpConns[i+1:]...)
	}

	// Give active connections a chance to close gracefully
	time.Sleep(100 * time.Millisecond)

	return nil
}

func (pm *ProxyManager) startTarget(proto, listenIP string, port int, targetAddr string) error {
	switch proto {
	case "tcp":
		var listener net.Listener
		if pm.tnet != nil {
			l, err := pm.tnet.ListenTCP(&net.TCPAddr{Port: port})
			if err != nil {
				return fmt.Errorf("failed to create TCP listener: %v", err)
			}
			listener = l
		} else if pm.nativeListenIP != "" {
			l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP(pm.nativeListenIP), Port: port})
			if err != nil {
				return fmt.Errorf("failed to create native TCP listener on %s:%d: %v", pm.nativeListenIP, port, err)
			}
			listener = l
		} else {
			return fmt.Errorf("proxy manager has no tnet or native IP configured")
		}
		ml := newManagedListener(listener)
		pm.listeners = append(pm.listeners, ml)
		go pm.handleTCPProxy(ml, targetAddr)

	case "udp":
		var conn net.PacketConn
		if pm.tnet != nil {
			c, err := pm.tnet.ListenUDP(&net.UDPAddr{Port: port})
			if err != nil {
				return fmt.Errorf("failed to create UDP listener: %v", err)
			}
			conn = c
		} else if pm.nativeListenIP != "" {
			c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(pm.nativeListenIP), Port: port})
			if err != nil {
				return fmt.Errorf("failed to create native UDP listener on %s:%d: %v", pm.nativeListenIP, port, err)
			}
			conn = c
		} else {
			return fmt.Errorf("proxy manager has no tnet or native IP configured")
		}
		mc := newManagedPacketConn(conn)
		pm.udpConns = append(pm.udpConns, mc)
		go pm.handleUDPProxy(mc, targetAddr)

	default:
		return fmt.Errorf(errUnsupportedProtoFmt, proto)
	}

	logger.Info("Started %s proxy to %s", proto, targetAddr)
	logger.Debug("Started %s proxy from %s:%d to %s", proto, listenIP, port, targetAddr)

	return nil
}

func (pm *ProxyManager) handleTCPProxy(listener *managedListener, targetAddr string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-listener.closed:
				logger.Info("TCP listener closed, stopping proxy handler for %v", listener.Addr())
				return
			default:
			}
			if !pm.running {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				logger.Info("TCP listener closed, stopping proxy handler for %v", listener.Addr())
				return
			}
			logger.Error("Error accepting TCP connection: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Drop connection if blocking is enabled
		if pm.blocked.Load() {
			conn.Close()
			logger.Debug("TCP proxy: connection dropped (blocking enabled)")
			continue
		}

		go func(accepted net.Conn) {
			target, err := net.Dial("tcp", targetAddr)
			if err != nil {
				logger.Error("Error connecting to target: %v", err)
				accepted.Close()
				return
			}

			var wg sync.WaitGroup
			wg.Add(2)

			go func() {
				defer wg.Done()
				_, _ = io.Copy(target, accepted)
				_ = target.Close()
			}()

			go func() {
				defer wg.Done()
				_, _ = io.Copy(accepted, target)
				_ = accepted.Close()
			}()

			wg.Wait()
		}(conn)
	}
}

func (pm *ProxyManager) handleUDPProxy(conn *managedPacketConn, targetAddr string) {
	bufPtr := getUDPBuffer()
	defer putUDPBuffer(bufPtr)
	buffer := *bufPtr
	clientConns := make(map[string]*net.UDPConn)
	var clientsMutex sync.RWMutex

	for {
		n, remoteAddr, err := conn.ReadFrom(buffer)
		if err != nil {
			closeAllClients := func() {
				clientsMutex.Lock()
				for _, targetConn := range clientConns {
					targetConn.Close()
				}
				clientConns = nil
				clientsMutex.Unlock()
			}

			// Check for intentional closure first: netstack does not
			// surface net.ErrClosed/io.EOF from ReadFrom() after Close(),
			// so this channel is the only reliable signal.
			select {
			case <-conn.closed:
				logger.Info("UDP connection closed, stopping proxy handler")
				closeAllClients()
				return
			default:
			}

			if !pm.running {
				closeAllClients()
				return
			}

			// Check for connection closed conditions
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				logger.Info("UDP connection closed, stopping proxy handler")
				closeAllClients()
				return
			}

			logger.Error("Error reading UDP packet: %v", err)
			// Avoid a tight busy-loop if this error persists.
			time.Sleep(100 * time.Millisecond)
			continue
		}

		clientKey := remoteAddr.String()
		// Drop packet if blocking is enabled
		if pm.blocked.Load() {
			logger.Debug("UDP proxy: packet dropped (blocking enabled)")
			continue
		}
		clientsMutex.RLock()
		targetConn, exists := clientConns[clientKey]
		clientsMutex.RUnlock()

		if !exists {
			targetUDPAddr, err := net.ResolveUDPAddr("udp", targetAddr)
			if err != nil {
				logger.Error("Error resolving target address: %v", err)
				continue
			}

			targetConn, err = net.DialUDP("udp", nil, targetUDPAddr)
			if err != nil {
				logger.Error("Error connecting to target: %v", err)
				continue
			}
			// Prevent idle UDP client goroutines from living forever and
			// retaining large per-connection buffers.
			_ = targetConn.SetReadDeadline(time.Now().Add(pm.udpIdleTimeout))

			clientsMutex.Lock()
			clientConns[clientKey] = targetConn
			clientsMutex.Unlock()

			go func(clientKey string, targetConn *net.UDPConn, remoteAddr net.Addr) {
				bufPtr := getUDPBuffer()
				defer func() {
					// Return buffer to pool first
					putUDPBuffer(bufPtr)
					// Always clean up when this goroutine exits
					clientsMutex.Lock()
					if storedConn, exists := clientConns[clientKey]; exists && storedConn == targetConn {
						delete(clientConns, clientKey)
						targetConn.Close()
					}
					clientsMutex.Unlock()
				}()

				buffer := *bufPtr
				for {
					n, _, err := targetConn.ReadFromUDP(buffer)
					if err != nil {
						var netErr net.Error
						if errors.As(err, &netErr) && netErr.Timeout() {
							return
						}
						// Connection closed is normal during cleanup
						if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
							return // defer will handle cleanup
						}
						logger.Error("Error reading from target: %v", err)
						return // defer will handle cleanup
					}

					_, err = conn.WriteTo(buffer[:n], remoteAddr)
					if err != nil {
						logger.Error("Error writing to client: %v", err)
						return // defer will handle cleanup
					}
				}
			}(clientKey, targetConn, remoteAddr)
		}

		written, err := targetConn.Write(buffer[:n])
		if err != nil {
			logger.Error("Error writing to target: %v", err)
			targetConn.Close()
			clientsMutex.Lock()
			delete(clientConns, clientKey)
			clientsMutex.Unlock()
		} else if written > 0 {
			// Extend idle timeout whenever client traffic is observed.
			_ = targetConn.SetReadDeadline(time.Now().Add(pm.udpIdleTimeout))
		}
	}
}

// write a function to print out the current targets in the ProxyManager
func (pm *ProxyManager) PrintTargets() {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	logger.Info("Current TCP Targets:")
	for listenIP, targets := range pm.tcpTargets {
		for port, targetAddr := range targets {
			logger.Info("TCP %s:%d -> %s", listenIP, port, targetAddr)
		}
	}

	logger.Info("Current UDP Targets:")
	for listenIP, targets := range pm.udpTargets {
		for port, targetAddr := range targets {
			logger.Info("UDP %s:%d -> %s", listenIP, port, targetAddr)
		}
	}
}

// GetTargets returns a copy of the current TCP and UDP targets
// Returns map[listenIP]map[port]targetAddress for both TCP and UDP
func (pm *ProxyManager) GetTargets() (tcpTargets map[string]map[int]string, udpTargets map[string]map[int]string) {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	tcpTargets = make(map[string]map[int]string)
	for listenIP, targets := range pm.tcpTargets {
		tcpTargets[listenIP] = make(map[int]string)
		for port, targetAddr := range targets {
			tcpTargets[listenIP][port] = targetAddr
		}
	}

	udpTargets = make(map[string]map[int]string)
	for listenIP, targets := range pm.udpTargets {
		udpTargets[listenIP] = make(map[int]string)
		for port, targetAddr := range targets {
			udpTargets[listenIP][port] = targetAddr
		}
	}

	return tcpTargets, udpTargets
}
