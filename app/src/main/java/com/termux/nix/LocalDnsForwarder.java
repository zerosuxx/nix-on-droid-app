package com.termux.nix;

import android.net.DnsResolver;
import android.os.Build;

import androidx.annotation.NonNull;
import androidx.annotation.Nullable;
import androidx.annotation.RequiresApi;

import com.termux.shared.logger.Logger;

import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.net.DatagramPacket;
import java.net.DatagramSocket;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.SocketTimeoutException;
import java.util.Arrays;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.Executor;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;

/**
 * NIX-ON-DROID: A DNS forwarder on 127.0.0.1:{@link #PORT} (UDP and TCP) that resolves through
 * Android's own resolver.
 *
 * <p>Termux programs link against Bionic, whose getaddrinfo() asks netd, so they follow the
 * network the app is on: VPN DNS (e.g. Twingate split DNS), Private DNS, the system cache. The
 * glibc in Nix-on-Droid instead sends queries straight to the servers in /etc/resolv.conf and
 * cannot see any of that. With {@code nameserver 127.0.0.1} in resolv.conf and proot started with
 * {@code -p}, which rewrites ports below 1024 by +2000 for binds and for connects to localhost,
 * glibc's queries to port 53 land here and are passed unchanged to
 * {@link DnsResolver#rawQuery} on the app's default network.
 *
 * <p>Requires Android 10 (API 29) for {@link DnsResolver}; on older versions it does not start
 * and resolv.conf's next nameserver takes over immediately, since nothing listens on the port.
 */
public final class LocalDnsForwarder {

    private static final String LOG_TAG = "LocalDnsForwarder";

    /** proot -p maps port 53 to 53 + 2000. */
    public static final int PORT = 2053;

    private static final int HEADER_SIZE = 12;
    /** Largest UDP answer a client without EDNS accepts (RFC 1035). */
    private static final int MAX_PLAIN_UDP_SIZE = 512;
    private static final int RCODE_SERVFAIL = 2;
    /** Close idle TCP clients, and give up on an answer, after this long. */
    private static final int TCP_TIMEOUT_MS = 30_000;

    private static boolean sStarted;

    private LocalDnsForwarder() {}

    public static synchronized void start() {
        if (sStarted) return;
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) {
            Logger.logInfo(LOG_TAG, "Not started: DnsResolver needs Android 10 or newer");
            return;
        }
        sStarted = true;
        Executor executor = Executors.newCachedThreadPool();
        new Thread(() -> serveUdp(executor), "dns-forwarder-udp").start();
        new Thread(() -> serveTcp(executor), "dns-forwarder-tcp").start();
    }

    @RequiresApi(api = Build.VERSION_CODES.Q)
    private static void serveUdp(@NonNull Executor executor) {
        try (DatagramSocket socket = new DatagramSocket(new InetSocketAddress(InetAddress.getLoopbackAddress(), PORT))) {
            Logger.logInfo(LOG_TAG, "Listening on udp 127.0.0.1:" + PORT);
            byte[] buffer = new byte[65535];
            while (true) {
                DatagramPacket packet = new DatagramPacket(buffer, buffer.length);
                socket.receive(packet);
                byte[] query = Arrays.copyOfRange(packet.getData(), packet.getOffset(), packet.getOffset() + packet.getLength());
                InetSocketAddress client = (InetSocketAddress) packet.getSocketAddress();
                if (query.length < HEADER_SIZE) continue;

                resolve(query, executor, answer -> {
                    // Without EDNS the client only takes 512 bytes: send TC so it retries over TCP.
                    if (answer.length > MAX_PLAIN_UDP_SIZE && additionalCount(query) == 0)
                        answer = truncated(query);
                    try {
                        socket.send(new DatagramPacket(answer, answer.length, client));
                    } catch (IOException e) {
                        Logger.logDebug(LOG_TAG, "UDP reply failed: " + e.getMessage());
                    }
                });
            }
        } catch (IOException e) {
            Logger.logStackTraceWithMessage(LOG_TAG, "UDP listener stopped", e);
        }
    }

    @RequiresApi(api = Build.VERSION_CODES.Q)
    private static void serveTcp(@NonNull Executor executor) {
        try (ServerSocket server = new ServerSocket()) {
            server.bind(new InetSocketAddress(InetAddress.getLoopbackAddress(), PORT));
            Logger.logInfo(LOG_TAG, "Listening on tcp 127.0.0.1:" + PORT);
            while (true) {
                Socket client = server.accept();
                executor.execute(() -> serveTcpClient(client, executor));
            }
        } catch (IOException e) {
            Logger.logStackTraceWithMessage(LOG_TAG, "TCP listener stopped", e);
        }
    }

    /** DNS over TCP: each message is prefixed with its 16-bit length; a client may send several. */
    @RequiresApi(api = Build.VERSION_CODES.Q)
    private static void serveTcpClient(@NonNull Socket client, @NonNull Executor executor) {
        try (Socket socket = client) {
            socket.setSoTimeout(TCP_TIMEOUT_MS);
            DataInputStream in = new DataInputStream(socket.getInputStream());
            DataOutputStream out = new DataOutputStream(socket.getOutputStream());
            while (true) {
                byte[] query = new byte[in.readUnsignedShort()];
                in.readFully(query);
                if (query.length < HEADER_SIZE) return;

                BlockingQueue<byte[]> result = new ArrayBlockingQueue<>(1);
                resolve(query, executor, result::offer);
                byte[] answer = result.poll(TCP_TIMEOUT_MS, TimeUnit.MILLISECONDS);
                if (answer == null) return;
                out.writeShort(answer.length);
                out.write(answer);
                out.flush();
            }
        } catch (EOFException | SocketTimeoutException ignored) {
            // Client closed the connection or went idle.
        } catch (IOException | InterruptedException e) {
            Logger.logDebug(LOG_TAG, "TCP client failed: " + e.getMessage());
        }
    }

    private interface AnswerCallback {
        void onAnswer(@NonNull byte[] answer);
    }

    /** Resolve on the app's default network (the VPN when one is up); reply SERVFAIL on errors. */
    @RequiresApi(api = Build.VERSION_CODES.Q)
    private static void resolve(@NonNull byte[] query, @NonNull Executor executor, @NonNull AnswerCallback callback) {
        try {
            DnsResolver.getInstance().rawQuery(null, query, DnsResolver.FLAG_EMPTY, executor, null,
                new DnsResolver.Callback<byte[]>() {
                    @Override
                    public void onAnswer(@NonNull byte[] answer, int rcode) {
                        callback.onAnswer(answer);
                    }

                    @Override
                    public void onError(@NonNull DnsResolver.DnsException error) {
                        Logger.logDebug(LOG_TAG, "Query failed: " + error.getMessage());
                        callback.onAnswer(errorResponse(query, RCODE_SERVFAIL, false));
                    }
                });
        } catch (RuntimeException e) {
            // rawQuery rejects malformed queries synchronously.
            Logger.logDebug(LOG_TAG, "Query rejected: " + e.getMessage());
            callback.onAnswer(errorResponse(query, RCODE_SERVFAIL, false));
        }
    }

    private static int additionalCount(@NonNull byte[] message) {
        return ((message[10] & 0xff) << 8) | (message[11] & 0xff);
    }

    @NonNull
    private static byte[] truncated(@NonNull byte[] query) {
        return errorResponse(query, 0, true);
    }

    /**
     * A response carrying only the query's header and question, as resolvers (glibc included)
     * drop answers whose question does not match the query.
     */
    @NonNull
    private static byte[] errorResponse(@NonNull byte[] query, int rcode, boolean truncated) {
        Integer questionEnd = questionEnd(query);
        byte[] response = Arrays.copyOf(query, questionEnd != null ? questionEnd : HEADER_SIZE);
        // QR, keep opcode and RD, set TC if asked; RA plus the rcode.
        response[2] = (byte) (0x80 | (query[2] & 0x79) | (truncated ? 0x02 : 0));
        response[3] = (byte) (0x80 | (rcode & 0x0f));
        if (questionEnd == null) {
            response[4] = 0;
            response[5] = 0;
        }
        // No answer, authority or additional records.
        Arrays.fill(response, 6, HEADER_SIZE, (byte) 0);
        return response;
    }

    /** End offset of the single question (QNAME, QTYPE, QCLASS), or null if malformed. */
    @Nullable
    private static Integer questionEnd(@NonNull byte[] query) {
        if (((query[4] & 0xff) << 8 | (query[5] & 0xff)) != 1) return null;
        int offset = HEADER_SIZE;
        while (offset < query.length) {
            int length = query[offset] & 0xff;
            if (length == 0) {
                int end = offset + 1 + 4;
                return end <= query.length ? end : null;
            }
            // Queries never use compression pointers in the question.
            if ((length & 0xc0) != 0) return null;
            offset += 1 + length;
        }
        return null;
    }

}
