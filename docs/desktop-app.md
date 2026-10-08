# Kivali Desktop

Kivali Desktop runs your teams on your computer and opens teams that run elsewhere. This page covers teams and their windows, the menu bar and the notification area, notifications, updates, pausing and resuming, resources, opening a team from other computers, and uninstalling.

Kivali Desktop runs on macOS 13 or later on Apple silicon, and on 64-bit Windows 10 or 11 Pro, Enterprise or Education with Hyper-V turned on. It works the same way on both; where they differ, this page says so. Keyboard shortcuts are written for macOS: on Windows, use Ctrl where macOS uses ⌘.

## Teams

A **team** is one group of agents with its own files, memory and settings. Each team on this computer runs in its own small virtual machine, separate from your other teams and from the rest of your computer. You can have several teams, for example one for work and one for home.

- **New team…** (File menu, ⌘N) creates another team, with the same five steps as your first. See [Getting started](getting-started.md#2-create-a-team).
- **Connect to a team…** (⇧⌘N) adds a team that runs on another computer or a server, by its address.

Your agents work only while their team is running, and a team here runs only while your computer is awake and Kivali is open.

## Windows

Each team opens in its own window, showing that team's Kivali. When a team cannot show itself (paused, waking up, updating, or it could not start), its window says so and offers the next step, such as **Resume** or **Try again**.

**Open in browser** (Team menu, ⇧⌘B, or the team's **Overview** in Settings) opens the running team in your usual browser on this computer.

Closing a window does not stop anything. Kivali keeps running in the menu bar on macOS, and in the notification area on Windows.

## The Kivali icon and the menus

Kivali's icon, in the menu bar on macOS and in the notification area on Windows, lists every team with its state ("3 agents working", "paused", "Claude isn't signed in"). Choose a team to open its window. The icon's menu also offers **Open Kivali**, **New team…**, **Connect to a team…**, **Settings…**, an update when one is ready, and **Quit Kivali**.

The menus add:

- **Kivali** (macOS): **About Kivali**, **Open source licenses**, **Settings…** and **Check for updates…**. On Windows, **Settings…**, **Check for updates…** and **Quit Kivali** are in the **File** menu, and **Open source licenses** is in **Help**.
- **Team**, while a team's window is in front: **Pause** or **Resume** that team, its settings, and **Open in browser**.
- **View**: **Reload**, **Actual size**, **Zoom in** and **Zoom out**, and on macOS **Enter full screen**.
- **Help**: **Kivali help** (these docs), **What's new** (the release notes), and **Show logs**.

On macOS the menus are in the menu bar at the top of the screen. On Windows each team window has its own.

## Settings

**Settings…** (⌘,) has a **General** page, one page per team, and **Advanced**.

**General**

- **Open Kivali at login**. Teams that were running when you quit resume too.
- **Ask before quitting**. Quitting pauses every team on this computer.
- The Kivali app's version and **Check for updates**.

**Each team** (a team on this computer) has four tabs:

| Tab | What it holds |
| --- | --- |
| **Overview** | Status with **Pause…** or **Resume**, the team's Kivali version and updates, **Open in browser**, and **Delete**. |
| **AI** | How agents sign in to Claude: the account and what it bills, such as **Claude Max** or **Amazon Bedrock**, as Claude Code reports them. Before the first sign-in, **Sign in…** opens Claude's sign-in in a terminal window. Once signed in, **Sign in again…** opens Claude in a terminal window, where `/login` switches account or billing; it first removes any Microsoft Foundry settings, which would otherwise outrank the new sign-in. **Use Microsoft Foundry**, **Set up…** (**Change…** once set up) connects a Foundry resource through a form, since Claude's sign-in has none, and checks each model's deployment. Agents use a new sign-in from their next turn. |
| **This Mac** (**This PC** on Windows) | Memory and CPUs for the team, how much of your computer's memory running teams use, and the team's disk use. |
| **Other devices** | Let other computers connect to the team. See below. |

A team elsewhere shows its connection, who you are signed in as (**Sign out**), and **Remove**, which takes it off this computer and leaves it running where it is.

**Advanced** opens the logs and the settings folder, and saves a diagnostics file (logs and settings, with no team data and no sign-ins) for when you need help.

## Pausing and resuming

**Pause** a team to stop its agents and free its memory. Agents that are working stop mid-task and pick up where they left off when you **Resume**. Nothing is lost.

**Quit Kivali** pauses every team on this computer. With **Ask before quitting** on, Kivali asks first. Teams that were running when you quit resume the next time Kivali opens.

If a team does not fit in your computer's free memory, Kivali offers to pause another team to make room.

## Notifications

When Kivali is not the app in front, it notifies you when:

- a team stopped unexpectedly;
- Claude signed a team out, so its agents cannot work until you sign in again;
- a team finished updating, or could not update.

Click a notification to go straight to the team or the setting it concerns.

## Updates

There are two things to keep current: the Kivali app, and each team's Kivali.

**The app** checks for updates each time it opens; **Check for updates…** checks now. When one is ready, choose **Update…**. Kivali asks to restart; running teams pause and resume after the restart.

**Each team** checks when it starts and once a day while it runs. When a new version is available, the team's **Overview** in Settings (and the Kivali icon's menu) offers **Update…**, with **What's new**. An update takes a few minutes, during which agents pause, and they pick up where they left off. If anything goes wrong, the team goes back to the version it was on.

Sometimes a team update needs a newer app first ("update this app first, in General"), or needs a few steps by hand; the team's **Overview** says which.

## Resources

Each team has its own memory and CPU allowance, set in Settings, under the team's **This Mac** (**This PC**) tab:

- **Memory**: 4 GB by default, up to half your computer's memory or 16 GB, whichever is less.
- **CPUs**: 4 by default.

Changes apply the next time the team resumes. The tab shows how much of your computer's memory the running teams use together.

Each team's disk can grow to 64 GB, but only what it uses takes space on your computer. The same tab shows how much it uses.

## Open a team from other computers

A team on this computer is reachable from this computer only, until you choose otherwise.

To open it from another computer, turn on **Let other computers connect to** your team under **Other devices**, and give it an **https** address that the other computer can reach. Kivali does not set up that address for you: put the team behind https yourself, for example with `tailscale serve` pointing at the team's local address, which the tab shows. Choose **Check and save**.

On the other computer, in Kivali Desktop, choose **File**, **Connect to a team…**, paste the address, and sign in with the same Google account you use on this computer. You can also open the address in any browser. See [Sign-in](sign-in.md). Other devices reach the team only while this computer is awake, the team is running, and your https address is serving.

## Where your teams live

Everything Kivali Desktop keeps is in one folder: `~/Library/Application Support/Kivali` on macOS, `%LOCALAPPDATA%\Kivali` on Windows. It holds the list of teams, each team's disk, and its logs. A team's agents, files and chats live on that team's disk, inside its virtual machine. To get a copy you can open or move, take a [backup](backup-and-restore.md).

## Deleting a team

In Settings, on the team's **Overview**, choose the **Delete** button, which names the team. Kivali lists what goes: the agents and what they remember, the files, every chat and assignment, the Claude sign-in, and the team's disk. Choose **Continue…**, type the team's name, choose **Delete forever**, and confirm it is you: with your password or Touch ID on macOS, with Windows Hello on Windows (or a confirmation, where Windows Hello is not set up). This cannot be undone, so take a backup first if you might want the team again.

## Uninstalling

1. Take a backup of any team you want to keep.
2. Delete each team in Settings, as above.
3. Quit Kivali, then remove the app. On macOS, move it from **Applications** to the Trash. On Windows, open **Settings**, **Apps**, **Installed apps**, and uninstall **Kivali**.
4. To remove everything else Kivali kept (logs, settings), delete its folder: `~/Library/Application Support/Kivali` on macOS, `%LOCALAPPDATA%\Kivali` on Windows.

If **Open Kivali at login** was on, turn it off before uninstalling. On macOS you can also remove Kivali from **System Settings**, **General**, **Login Items**.
